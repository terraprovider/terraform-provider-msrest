package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
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
}

const plannerPath = "/taskAPI/tenantAdminSettings/Settings"

// mockAPI is a minimal settings API: GET reads an object, PATCH merges into it,
// PUT sets the given properties, DELETE removes it. After each write it serves
// the previous value for `lag` more GETs, like the eventually consistent
// Microsoft APIs.
type mockAPI struct {
	mu        sync.Mutex
	objects   map[string]map[string]any
	stale     map[string]map[string]any
	staleLeft map[string]int
	lag       int
	requests  []*http.Request
	failWith  string // if set, writes fail with this 400 body
	throttle  int    // answer this many upcoming GETs with 429
	throttled int    // number of 429s sent
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
			"/v1/policies/AllowSelfServicePurchase/products/P1": {"productId": "P1", "productName": "Product 1", "policyValue": "Enabled"},
			"/v1/policies/AllowSelfServicePurchase/products/P2": {"productId": "P2", "productName": "Product 2", "policyValue": "Enabled"},
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
	m.requests = append(m.requests, r.Clone(r.Context()))
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
		if m.failWith != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, m.failWith)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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

func (m *mockAPI) checkValue(path, key string, want any) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := m.get(path, key); got != want {
			return fmt.Errorf("%s %s = %v, want %v", path, key, got, want)
		}
		return nil
	}
}

func (m *mockAPI) checkHeader(method, key, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, r := range m.requests {
			if r.Method == method && r.Header.Get(key) == want {
				return nil
			}
		}
		return fmt.Errorf("no %s request with %s: %s", method, key, want)
	}
}

const providerBlock = `
provider "msrest" {
  access_token = "test-token"
}
`

func plannerConfig(base string, allowCalendarSharing bool) string {
	return providerBlock + fmt.Sprintf(`
resource "msrest_object" "planner" {
  url     = "%s%s"
  headers = { "OData-Version" = "4.0;NetFx" }
  body    = jsonencode({ allowCalendarSharing = %t })
}
`, base, plannerPath, allowCalendarSharing)
}

func TestObjectResourcePlanner(t *testing.T) {
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
					resource.TestCheckResourceAttr("msrest_object.planner", "id", url),
					resource.TestCheckResourceAttr("msrest_object.planner", "method", "PATCH"),
					resource.TestCheckResourceAttr("msrest_object.planner", "body", `{"allowCalendarSharing":false}`),
					resource.TestCheckResourceAttrWith("msrest_object.planner", "response", func(v string) error {
						if !strings.Contains(v, `"allowCalendarSharing":false`) || !strings.Contains(v, `"isPlannerAllowed":true`) {
							return fmt.Errorf("unexpected response %s", v)
						}
						return nil
					}),
					api.checkValue(plannerPath, "allowCalendarSharing", false),
					api.checkHeader(http.MethodPatch, "OData-Version", "4.0;NetFx"),
					api.checkHeader(http.MethodPatch, "Content-Type", "application/json"),
				),
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
			},
			{
				Config: plannerConfig(srv.URL, false),
			},
			{
				ResourceName:            "msrest_object.planner",
				ImportState:             true,
				ImportStateId:           url,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"body", "headers", "response"},
			},
		},
	})
}

func TestObjectResourceSelfServicePurchase(t *testing.T) {
	api, srv := newMockAPI(t)
	config := providerBlock + fmt.Sprintf(`
resource "msrest_object" "purchase" {
  count         = 2
  url           = "%s/v1/policies/AllowSelfServicePurchase/products/P${count.index + 1}"
  scope         = "aeb86249-8ea3-49e2-900b-54cc8e308f85/.default"
  method        = "PUT"
  body          = jsonencode({ policyValue = "Disabled" })
  delete_method = "PUT"
  delete_body   = jsonencode({ policyValue = "Enabled" })
}
`, srv.URL)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			api.checkValue("/v1/policies/AllowSelfServicePurchase/products/P1", "policyValue", "Enabled"),
			api.checkValue("/v1/policies/AllowSelfServicePurchase/products/P2", "policyValue", "Enabled"),
		),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					api.checkValue("/v1/policies/AllowSelfServicePurchase/products/P1", "policyValue", "Disabled"),
					api.checkValue("/v1/policies/AllowSelfServicePurchase/products/P2", "policyValue", "Disabled"),
				),
			},
		},
	})
}

func TestObjectResourceGoneIsRecreated(t *testing.T) {
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

func TestObjectResourceWriteError(t *testing.T) {
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

func TestObjectResourceValidation(t *testing.T) {
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: providerBlock + `
resource "msrest_object" "x" {
  url  = "https://example.com/x"
  body = "{not json"
}`,
				ExpectError: regexp.MustCompile(`Invalid JSON`),
			},
			{
				Config: providerBlock + `
resource "msrest_object" "x" {
  url  = "example.com/x"
  body = "{}"
}`,
				ExpectError: regexp.MustCompile(`Invalid URL`),
			},
			{
				Config: providerBlock + `
resource "msrest_object" "x" {
  url         = "https://example.com/x"
  body        = "{}"
  delete_body = "{}"
}`,
				ExpectError: regexp.MustCompile(`delete_method`),
			},
		},
	})
}

func TestObjectDataSource(t *testing.T) {
	_, srv := newMockAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: providerBlock + fmt.Sprintf(`
data "msrest_object" "planner" {
  url = "%s%s"
}

output "allow_calendar_sharing" {
  value = jsondecode(data.msrest_object.planner.response).allowCalendarSharing
}
`, srv.URL, plannerPath),
				Check: resource.TestCheckOutput("allow_calendar_sharing", "true"),
			},
		},
	})
}

func TestObjectResourceThrottledReadAfterWrite(t *testing.T) {
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
					resource.TestCheckResourceAttrWith("msrest_object.planner", "response", func(v string) error {
						if !strings.Contains(v, `"allowCalendarSharing":false`) {
							return fmt.Errorf("unexpected response %s", v)
						}
						return nil
					}),
					func(*terraform.State) error {
						api.mu.Lock()
						defer api.mu.Unlock()
						if api.throttled < 5 {
							return fmt.Errorf("only %d requests were throttled", api.throttled)
						}
						return nil
					},
				),
			},
		},
	})
}
