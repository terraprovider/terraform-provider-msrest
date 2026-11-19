package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/terraprovider/go-msadmin/retry"
)

// These tests run the provider through Terraform/OpenTofu against an in-memory
// mock API, so they need no tenant. They need a terraform or tofu binary; with
// OpenTofu set TF_ACC_TERRAFORM_PATH=$(which tofu) and
// TF_ACC_PROVIDER_HOST=registry.opentofu.org.

var testProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"msrest": providerserver.NewProtocol6WithError(New("test")()),
}

func init() {
	pollInterval = 10 * time.Millisecond
	retryConfig = retry.Config{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
	conflictRetryConfig = retry.Config{MaxAttempts: 10, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

const (
	plannerPath  = "/taskAPI/tenantAdminSettings/Settings"
	nestedPath   = "/nested"
	plannerAddr  = "msrest_update_resource.planner"
	purchaseBase = "/v1/policies/AllowSelfServicePurchase/products/"
)

// recordedRequest is a request the mock API received, with its body.
type recordedRequest struct {
	method string
	path   string
	header http.Header
	body   string
}

// mockAPI is a minimal settings API: GET reads an object, PATCH merges into it,
// PUT sets the given properties, DELETE removes it. After each write it serves
// the previous value for `lag` more GETs, like the eventually consistent
// Microsoft APIs.
type mockAPI struct {
	mu         sync.Mutex
	objects    map[string]map[string]any
	stale      map[string]map[string]any
	staleLeft  map[string]int
	lag        int
	requests   []recordedRequest
	failWith   string // if set, writes fail with this 400 body
	throttle   int    // answer this many upcoming GETs with 429
	throttled  int    // number of 429s sent
	conflicts  int    // answer this many upcoming writes with 412, like the licensing API's colliding writes
	conflicted int    // number of 412s sent
}

func newMockAPI(t *testing.T) (*mockAPI, *httptest.Server) {
	m := &mockAPI{
		objects: map[string]map[string]any{
			plannerPath: {
				"@odata.context":       "https://tasks.office.com/taskapi/tenantAdminSettings/$metadata#Settings",
				"id":                   "1",
				"isPlannerAllowed":     true,
				"allowCalendarSharing": true,
			},
			purchaseBase + "P1": {"productId": "P1", "productName": "Product 1", "policyValue": "Enabled"},
			purchaseBase + "P2": {"productId": "P2", "productName": "Product 2", "policyValue": "Enabled"},
			nestedPath: {
				"settings": map[string]any{"a": 1.0, "list": []any{"x", "y"}, "extra": "unmanaged"},
				"ratio":    0.5,
				"flag":     true,
			},
		},
		stale:     map[string]map[string]any{},
		staleLeft: map[string]int{},
	}
	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)
	return m, srv
}

func clone(o map[string]any) map[string]any {
	c := make(map[string]any, len(o))
	for k, v := range o {
		c[k] = v
	}
	return c
}

func (m *mockAPI) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, _ := io.ReadAll(r.Body)
	m.requests = append(m.requests, recordedRequest{r.Method, r.URL.Path, r.Header.Clone(), string(data)})
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && m.throttle > 0 {
		m.throttle--
		m.throttled++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"","message":"Too many requests."}}`)
		return
	}
	obj, ok := m.objects[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"NotFound","message":"no such object"}}`)
		return
	}
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch r.Method {
	case http.MethodGet:
		if m.staleLeft[r.URL.Path] > 0 {
			m.staleLeft[r.URL.Path]--
			writeJSON(m.stale[r.URL.Path])
			return
		}
		writeJSON(obj)
	case http.MethodPatch, http.MethodPut:
		if m.conflicts > 0 {
			m.conflicts--
			m.conflicted++
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, cosmosConflict)
			return
		}
		if m.failWith != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, m.failWith)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.stale[r.URL.Path], m.staleLeft[r.URL.Path] = clone(obj), m.lag
		for k, v := range body {
			obj[k] = v
		}
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(obj)
	case http.MethodDelete:
		delete(m.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (m *mockAPI) get(path, key string) any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objects[path]; ok {
		return o[key]
	}
	return nil
}

func (m *mockAPI) set(path, key string, v any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[path][key] = v
	m.staleLeft[path] = 0
}

func (m *mockAPI) remove(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, path)
}

// checkValue compares a property of an object, as JSON.
func (m *mockAPI) checkValue(path, key string, want any) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got, _ := json.Marshal(m.get(path, key))
		w, _ := json.Marshal(want)
		if string(got) != string(w) {
			return fmt.Errorf("%s %s = %s, want %s", path, key, got, w)
		}
		return nil
	}
}

func (m *mockAPI) checkHeader(method, key, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, r := range m.requests {
			if r.method == method && r.header.Get(key) == want {
				return nil
			}
		}
		return fmt.Errorf("no %s request with %s: %s", method, key, want)
	}
}

// lastRequest returns the last request with the given method.
func (m *mockAPI) lastRequest(method string) (recordedRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.requests) - 1; i >= 0; i-- {
		if m.requests[i].method == method {
			return m.requests[i], true
		}
	}
	return recordedRequest{}, false
}

// checkLastBody compares the raw body of the last request with the given method.
func (m *mockAPI) checkLastBody(method, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		r, ok := m.lastRequest(method)
		if !ok {
			return fmt.Errorf("no %s request", method)
		}
		if r.body != want {
			return fmt.Errorf("%s body = %q, want %q", method, r.body, want)
		}
		return nil
	}
}

const providerBlock = `
provider "msrest" {
  access_token = "test-token"
}
`

func plannerConfig(base string, allowCalendarSharing bool) string {
	return providerBlock + fmt.Sprintf(`
resource "msrest_update_resource" "planner" {
  url     = "%s%s"
  headers = { "OData-Version" = "4.0;NetFx" }
  body    = { allowCalendarSharing = %t }

  response_export_values = {
    all     = "@"
    sharing = "allowCalendarSharing"
  }
}
`, base, plannerPath, allowCalendarSharing)
}

func TestUpdateResourcePlanner(t *testing.T) {
	api, srv := newMockAPI(t)
	api.lag = 3 // the first reads after a write still return the old value
	url := srv.URL + plannerPath

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			// no delete_method: destroy leaves the setting alone
			api.checkValue(plannerPath, "allowCalendarSharing", false),
		),
		Steps: []resource.TestStep{
			{
				Config: plannerConfig(srv.URL, false),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(plannerAddr, "id", url),
					resource.TestCheckResourceAttr(plannerAddr, "update_method", "PATCH"),
					api.checkValue(plannerPath, "allowCalendarSharing", false),
					api.checkLastBody(http.MethodPatch, `{"allowCalendarSharing":false}`),
					api.checkHeader(http.MethodPatch, "OData-Version", "4.0;NetFx"),
					api.checkHeader(http.MethodPatch, "Content-Type", "application/json"),
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(plannerAddr, tfjsonpath.New("body"),
						knownvalue.ObjectExact(map[string]knownvalue.Check{"allowCalendarSharing": knownvalue.Bool(false)})),
					statecheck.ExpectKnownValue(plannerAddr, tfjsonpath.New("output").AtMapKey("sharing"), knownvalue.Bool(false)),
					statecheck.ExpectKnownValue(plannerAddr, tfjsonpath.New("output").AtMapKey("all").AtMapKey("isPlannerAllowed"), knownvalue.Bool(true)),
				},
			},
			{
				// A change made outside of Terraform shows up as drift ...
				PreConfig:          func() { api.set(plannerPath, "allowCalendarSharing", true) },
				Config:             plannerConfig(srv.URL, false),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// ... and the next apply corrects it.
				Config: plannerConfig(srv.URL, false),
				Check:  api.checkValue(plannerPath, "allowCalendarSharing", false),
			},
			{
				// Properties outside body are not managed.
				PreConfig: func() { api.set(plannerPath, "isPlannerAllowed", false) },
				Config:    plannerConfig(srv.URL, false),
				PlanOnly:  true,
			},
			{
				Config: plannerConfig(srv.URL, true),
				Check:  api.checkValue(plannerPath, "allowCalendarSharing", true),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(plannerAddr, plancheck.ResourceActionUpdate),
						// The exported values come from the response.
						plancheck.ExpectUnknownValue(plannerAddr, tfjsonpath.New("output")),
					},
				},
			},
			{
				Config: plannerConfig(srv.URL, false),
			},
			{
				ResourceName:            plannerAddr,
				ImportState:             true,
				ImportStateId:           url,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"body", "headers", "response_export_values", "output"},
			},
		},
	})
}

const purchaseAddr = "msrest_update_resource.purchase[0]"

func TestUpdateResourceSelfServicePurchase(t *testing.T) {
	api, srv := newMockAPI(t)
	config := func(value string) string {
		return providerBlock + fmt.Sprintf(`
resource "msrest_update_resource" "purchase" {
  count         = 2
  url           = "%s%sP${count.index + 1}"
  scope         = "aeb86249-8ea3-49e2-900b-54cc8e308f85/.default"
  update_method = "PUT"
  body          = { policyValue = %q }
  delete_method = "PUT"
  delete_body   = { policyValue = "Enabled" }
}
`, srv.URL, purchaseBase, value)
	}
	// Without response_export_values, output is an empty object, known at plan time.
	emptyOutput := []plancheck.PlanCheck{
		plancheck.ExpectKnownValue(purchaseAddr, tfjsonpath.New("output"), knownvalue.ObjectExact(map[string]knownvalue.Check{})),
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			api.checkValue(purchaseBase+"P1", "policyValue", "Enabled"),
			api.checkValue(purchaseBase+"P2", "policyValue", "Enabled"),
			api.checkLastBody(http.MethodPut, `{"policyValue":"Enabled"}`),
		),
		Steps: []resource.TestStep{
			{
				Config: config("Disabled"),
				Check: resource.ComposeTestCheckFunc(
					api.checkValue(purchaseBase+"P1", "policyValue", "Disabled"),
					api.checkValue(purchaseBase+"P2", "policyValue", "Disabled"),
				),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: emptyOutput},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(purchaseAddr, tfjsonpath.New("output"),
						knownvalue.ObjectExact(map[string]knownvalue.Check{})),
				},
			},
			{
				// An in-place update plans output = {}, not (known after apply).
				Config: config("OnlyTrialsWithoutPaymentMethod"),
				Check:  api.checkValue(purchaseBase+"P1", "policyValue", "OnlyTrialsWithoutPaymentMethod"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: append([]plancheck.PlanCheck{
						plancheck.ExpectResourceAction(purchaseAddr, plancheck.ResourceActionUpdate),
					}, emptyOutput...),
				},
			},
			{
				// The same after drift.
				PreConfig: func() { api.set(purchaseBase+"P1", "policyValue", "Enabled") },
				Config:    config("OnlyTrialsWithoutPaymentMethod"),
				Check:     api.checkValue(purchaseBase+"P1", "policyValue", "OnlyTrialsWithoutPaymentMethod"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: append([]plancheck.PlanCheck{
						plancheck.ExpectResourceAction(purchaseAddr, plancheck.ResourceActionUpdate),
					}, emptyOutput...),
				},
			},
		},
	})
}

// A body with nested objects, a list and numbers: no diff after the apply (the
// HCL types of the configuration are kept), drift in a nested value is found,
// and numbers are sent without exponent or trailing zeros.
func TestUpdateResourceNestedBody(t *testing.T) {
	api, srv := newMockAPI(t)
	config := providerBlock + fmt.Sprintf(`
resource "msrest_update_resource" "nested" {
  url  = "%s%s"
  body = {
    settings = { a = 2000000, list = ["x", "z"] }
    ratio    = 0.25
  }
}
`, srv.URL, nestedPath)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					api.checkLastBody(http.MethodPatch, `{"ratio":0.25,"settings":{"a":2000000,"list":["x","z"]}}`),
					api.checkValue(nestedPath, "ratio", 0.25),
				),
			},
			{
				PreConfig: func() {
					api.set(nestedPath, "settings", map[string]any{"a": 2000000.0, "list": []any{"x", "changed"}, "extra": "unmanaged"})
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				Check:  api.checkValue(nestedPath, "settings", map[string]any{"a": 2000000, "list": []any{"x", "z"}}),
			},
		},
	})
}

// A body that is not known until apply (it references another resource).
func TestUpdateResourceUnknownBody(t *testing.T) {
	api, srv := newMockAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: providerBlock + fmt.Sprintf(`
resource "terraform_data" "flag" {
  input = false
}

resource "msrest_update_resource" "planner" {
  url  = "%s%s"
  body = { allowCalendarSharing = terraform_data.flag.output }
}
`, srv.URL, plannerPath),
				Check: api.checkValue(plannerPath, "allowCalendarSharing", false),
			},
		},
	})
}

// delete_method without delete_body sends a request without a body.
func TestUpdateResourceDeleteWithoutBody(t *testing.T) {
	api, srv := newMockAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		CheckDestroy: func(*terraform.State) error {
			r, ok := api.lastRequest(http.MethodDelete)
			if !ok {
				return fmt.Errorf("no DELETE request")
			}
			if r.body != "" || r.header.Get("Content-Type") != "" {
				return fmt.Errorf("DELETE sent body %q with Content-Type %q, want neither", r.body, r.header.Get("Content-Type"))
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: providerBlock + fmt.Sprintf(`
resource "msrest_update_resource" "planner" {
  url           = "%s%s"
  body          = { allowCalendarSharing = false }
  delete_method = "DELETE"
}
`, srv.URL, plannerPath),
			},
		},
	})
}

func TestUpdateResourceGoneIsRecreated(t *testing.T) {
	api, srv := newMockAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{Config: plannerConfig(srv.URL, false)},
			{
				PreConfig:          func() { api.remove(plannerPath) },
				Config:             plannerConfig(srv.URL, false),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestUpdateResourceWriteError(t *testing.T) {
	api, srv := newMockAPI(t)
	api.failWith = `{"error":{"code":"BadRequest","message":"Invalid value for allowCalendarSharing"}}`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      plannerConfig(srv.URL, false),
				ExpectError: regexp.MustCompile(`Invalid\s+value\s+for\s+allowCalendarSharing\s+\(BadRequest,\s+http\s+400\)`),
			},
		},
	})
}

func TestUpdateResourceValidation(t *testing.T) {
	step := func(body, extra, wantErr string) resource.TestStep {
		return resource.TestStep{
			Config: providerBlock + fmt.Sprintf(`
resource "msrest_update_resource" "x" {
  url  = "https://example.com/x"
  body = %s
  %s
}`, body, extra),
			ExpectError: regexp.MustCompile(wantErr),
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			step(`jsonencode({ a = 1 })`, "", `not\s+a\s+JSON\s+string`),
			step(`1`, "", `Invalid body`),
			step(`{}`, `delete_body = {}`, `delete_method`),
			step(`{}`, `delete_method = "DELETE"`+"\n"+`delete_body = "x"`, `Invalid body`),
			step(`{}`, `response_export_values = { x = "a[" }`, `Invalid\s+JMESPath\s+expression`),
			{
				Config: providerBlock + `
resource "msrest_update_resource" "x" {
  url  = "example.com/x"
  body = {}
}`,
				ExpectError: regexp.MustCompile(`Invalid URL`),
			},
		},
	})
}

func TestResourceDataSource(t *testing.T) {
	_, srv := newMockAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: providerBlock + fmt.Sprintf(`
data "msrest_resource" "planner" {
  url = "%[1]s%[2]s"

  response_export_values = {
    all     = "@"
    sharing = "allowCalendarSharing"
  }
}

data "msrest_resource" "bare" {
  url = "%[1]s%[2]s"
}

output "allow_calendar_sharing" {
  value = data.msrest_resource.planner.output.sharing
}

output "planner_allowed" {
  value = data.msrest_resource.planner.output.all.isPlannerAllowed
}
`, srv.URL, plannerPath),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckOutput("allow_calendar_sharing", "true"),
					resource.TestCheckOutput("planner_allowed", "true"),
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("data.msrest_resource.bare", tfjsonpath.New("output"),
						knownvalue.ObjectExact(map[string]knownvalue.Check{})),
				},
			},
		},
	})
}

func TestUpdateResourceThrottledReadAfterWrite(t *testing.T) {
	api, srv := newMockAPI(t)
	api.lag = 1
	// More 429s than the transport retries (3 attempts in tests): the poll after
	// the write must keep going instead of giving up.
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig: func() { api.throttle = 5 },
				Config:    plannerConfig(srv.URL, false),
				Check: resource.ComposeTestCheckFunc(
					api.checkValue(plannerPath, "allowCalendarSharing", false),
					func(*terraform.State) error {
						api.mu.Lock()
						defer api.mu.Unlock()
						if api.throttled < 5 {
							return fmt.Errorf("only %d requests were throttled", api.throttled)
						}
						return nil
					},
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(plannerAddr, tfjsonpath.New("output").AtMapKey("sharing"), knownvalue.Bool(false)),
				},
			},
		},
	})
}

// Parallel writes to the licensing API collide on one Cosmos DB document and
// fail with 412 without writing anything (#14). The provider retries them.
func TestUpdateResourceConflictIsRetried(t *testing.T) {
	api, srv := newMockAPI(t)
	config := providerBlock + fmt.Sprintf(`
resource "msrest_update_resource" "purchase" {
  count         = 2
  url           = "%s%sP${count.index + 1}"
  scope         = "aeb86249-8ea3-49e2-900b-54cc8e308f85/.default"
  update_method = "PUT"
  body          = { policyValue = "Disabled" }
}
`, srv.URL, purchaseBase)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig: func() { api.conflicts = 6 },
				Config:    config,
				Check: resource.ComposeTestCheckFunc(
					api.checkValue(purchaseBase+"P1", "policyValue", "Disabled"),
					api.checkValue(purchaseBase+"P2", "policyValue", "Disabled"),
					func(*terraform.State) error {
						api.mu.Lock()
						defer api.mu.Unlock()
						if api.conflicted != 6 {
							return fmt.Errorf("%d writes were answered with 412, want 6", api.conflicted)
						}
						return nil
					},
				),
			},
		},
	})
}

// With an If-Match header, 412 means the object changed: no retry, an error.
func TestUpdateResourceConflictWithIfMatch(t *testing.T) {
	api, srv := newMockAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig: func() { api.conflicts = 1 },
				Config: providerBlock + fmt.Sprintf(`
resource "msrest_update_resource" "purchase" {
  url           = "%s%sP1"
  update_method = "PUT"
  headers       = { "If-Match" = "*" }
  body          = { policyValue = "Disabled" }
}
`, srv.URL, purchaseBase),
				// Only one 412 is queued, so a retry would have succeeded.
				ExpectError: regexp.MustCompile(`PreconditionFailed\s+\(412\)`),
			},
		},
	})
}

// writeTracker sits in front of the mock API and records how many writes (any
// method but GET) are in flight at once. Each write is held for a moment so
// that writes that are not serialized overlap. With barrier > 0, writes are
// instead held until `barrier` writes were in flight at once. With
// getsAfterWrites > 0, every GET is held until that many writes have arrived.
// Each hold ends after 5 s at the latest, and getTimedOut records it for GETs.
type writeTracker struct {
	next            http.Handler
	barrier         int
	getsAfterWrites int

	mu                       sync.Mutex
	active, maxActive, count int
	getTimedOut              bool
}

// waitFor polls cond for up to 5 s and reports whether it became true.
func (w *writeTracker) waitFor(cond func() bool) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		w.mu.Lock()
		ok := cond()
		w.mu.Unlock()
		if ok {
			return true
		}
	}
	return false
}

func (w *writeTracker) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if w.getsAfterWrites > 0 && !w.waitFor(func() bool { return w.count >= w.getsAfterWrites }) {
			w.mu.Lock()
			w.getTimedOut = true
			w.mu.Unlock()
		}
		w.next.ServeHTTP(rw, r)
		return
	}
	w.mu.Lock()
	w.active++
	w.count++
	w.maxActive = max(w.maxActive, w.active)
	w.mu.Unlock()
	if w.barrier > 0 {
		w.waitFor(func() bool { return w.maxActive >= w.barrier })
	} else {
		time.Sleep(20 * time.Millisecond)
	}
	w.next.ServeHTTP(rw, r)
	w.mu.Lock()
	w.active--
	w.mu.Unlock()
}

func (w *writeTracker) check(wantMax, wantCount int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.maxActive != wantMax || w.count != wantCount {
			return fmt.Errorf("%d writes, at most %d at once; want %d writes, at most %d at once", w.count, w.maxActive, wantCount, wantMax)
		}
		if w.getTimedOut {
			return fmt.Errorf("a GET waited 5 s for %d writes: a read after a write held the concurrency group", w.getsAfterWrites)
		}
		return nil
	}
}

func newTrackedMockAPI(t *testing.T, products, barrier int) (*mockAPI, *writeTracker, *httptest.Server) {
	api, _ := newMockAPI(t)
	for i := 1; i <= products; i++ {
		api.objects[fmt.Sprintf("%sP%d", purchaseBase, i)] = map[string]any{"productId": fmt.Sprintf("P%d", i), "policyValue": "Enabled"}
	}
	tracker := &writeTracker{next: http.HandlerFunc(api.serve), barrier: barrier}
	srv := httptest.NewServer(tracker)
	t.Cleanup(srv.Close)
	return api, tracker, srv
}

// Resources in the same concurrency group write one at a time, on create and
// on destroy with delete_method. The reads after a write do not hold the group:
// every GET is held until all n PUTs have arrived, which only happens if the
// next resource can write while the previous one polls.
func TestUpdateResourceConcurrencySerializesWrites(t *testing.T) {
	const n = 6
	api, tracker, srv := newTrackedMockAPI(t, n, 0)
	tracker.getsAfterWrites = n
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			tracker.check(1, 2*n),
			api.checkValue(purchaseBase+"P1", "policyValue", "Enabled"),
		),
		Steps: []resource.TestStep{
			{
				Config: providerBlock + fmt.Sprintf(`
resource "msrest_update_resource" "purchase" {
  count         = %d
  url           = "%s%sP${count.index + 1}"
  update_method = "PUT"
  body          = { policyValue = "Disabled" }
  delete_method = "PUT"
  delete_body   = { policyValue = "Enabled" }
  concurrency   = "licensing"
}
`, n, srv.URL, purchaseBase),
				Check: resource.ComposeTestCheckFunc(
					tracker.check(1, n),
					api.checkValue(purchaseBase+fmt.Sprintf("P%d", n), "policyValue", "Disabled"),
				),
			},
		},
	})
}

// Resources in different groups still write in parallel.
func TestUpdateResourceConcurrencyGroupsAreIndependent(t *testing.T) {
	_, tracker, srv := newTrackedMockAPI(t, 2, 2)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: providerBlock + fmt.Sprintf(`
resource "msrest_update_resource" "purchase" {
  count         = 2
  url           = "%s%sP${count.index + 1}"
  update_method = "PUT"
  body          = { policyValue = "Disabled" }
  concurrency   = "group-${count.index}"
}
`, srv.URL, purchaseBase),
				Check: tracker.check(2, 2),
			},
		},
	})
}

// concurrency is not part of the object: changing it alone writes nothing.
func TestUpdateResourceConcurrencyChangeWritesNothing(t *testing.T) {
	const addr = "msrest_update_resource.purchase"
	_, tracker, srv := newTrackedMockAPI(t, 0, 0)
	config := func(concurrency string) string {
		return providerBlock + fmt.Sprintf(`
resource "msrest_update_resource" "purchase" {
  url           = "%s%sP1"
  update_method = "PUT"
  body          = { policyValue = "Disabled" }
  response_export_values = { name = "productName" }
  %s
}
`, srv.URL, purchaseBase, concurrency)
	}
	nameIsKept := []statecheck.StateCheck{
		statecheck.ExpectKnownValue(addr, tfjsonpath.New("output").AtMapKey("name"), knownvalue.StringExact("Product 1")),
	}
	update := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
	}}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{Config: config(`concurrency = "a"`), Check: tracker.check(1, 1)},
			{Config: config(`concurrency = "b"`), Check: tracker.check(1, 1), ConfigPlanChecks: update, ConfigStateChecks: nameIsKept},
			{Config: config(``), Check: tracker.check(1, 1), ConfigPlanChecks: update, ConfigStateChecks: nameIsKept},
		},
	})
}
