package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"ai-engine/config"
)

func TestApplyKeyChangesRemovesByIndexThenAdds(t *testing.T) {
	got := applyKeyChanges([]string{"a", "b", "c"}, []string{"d", "a"}, []int{0, 2}, nil)
	want := []string{"b", "d", "a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestApplyKeyChangesReplaceWins(t *testing.T) {
	got := applyKeyChanges([]string{"a"}, []string{"x"}, []int{0}, []string{" new ", "new", ""})
	if !reflect.DeepEqual(got, []string{"new"}) {
		t.Errorf("got %v", got)
	}
}

func TestApplySessionAgentPatch(t *testing.T) {
	model := "m1"
	empty := ""
	yes := true
	got := applySessionAgentPatch(patchSessionAgentRequest{Model: &model, SubProvider: &empty, SubEnabled: &yes})
	want := map[string]any{"model": "m1", "sub_provider": "", "sub_enabled": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	cleared := applySessionAgentPatch(patchSessionAgentRequest{ClearModel: true, ClearSub: true})
	if cleared["provider"] != "" || cleared["model"] != "" || cleared["sub_model"] != "" {
		t.Errorf("clear flags not applied: %v", cleared)
	}
}

func TestRequireTokenGuardsEverythingButHealth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	guarded := RequireToken("secret", next)

	cases := []struct {
		path, auth string
		want       int
	}{
		{"/healthz", "", http.StatusOK},
		{"/api/providers", "", http.StatusUnauthorized},
		{"/api/providers", "Bearer wrong", http.StatusUnauthorized},
		{"/api/providers", "Bearer secret", http.StatusOK},
	}
	for _, c := range cases {
		request := httptest.NewRequest(http.MethodGet, c.path, nil)
		if c.auth != "" {
			request.Header.Set("Authorization", c.auth)
		}
		recorder := httptest.NewRecorder()
		guarded.ServeHTTP(recorder, request)
		if recorder.Code != c.want {
			t.Errorf("%s with %q: status %d, want %d", c.path, c.auth, recorder.Code, c.want)
		}
	}
}

func TestListProvidersShowsEveryProviderWithProbeState(t *testing.T) {
	gateway, _ := newTestGateway(t, newMemStore())
	config.Providers = []config.Provider{
		{ID: "1", Name: "alpha", APIURL: "https://a.example", Keys: []string{"k1", "k2"}, Adapter: "openai"},
		{ID: "2", Name: "beta", APIURL: "https://b.example", Keys: []string{"k"}, Adapter: "anthropic"},
	}
	gateway.probes.set("beta", probeResult{Probed: true, Reachable: true, ModelCount: 3, WorkingModel: "m"})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	gateway.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Providers []struct {
			ID         string `json:"id"`
			Adapter    string `json:"adapter"`
			KeyCount   int    `json:"key_count"`
			Reachable  bool   `json:"reachable"`
			ModelCount int    `json:"model_count"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Providers) != 2 {
		t.Fatalf("providers = %d, want 2 (multi-provider)", len(payload.Providers))
	}
	if payload.Providers[0].KeyCount != 2 || payload.Providers[0].Reachable {
		t.Errorf("alpha = %+v", payload.Providers[0])
	}
	if !payload.Providers[1].Reachable || payload.Providers[1].ModelCount != 3 {
		t.Errorf("beta = %+v", payload.Providers[1])
	}
}

func TestForEachBoundedNeverExceedsLimit(t *testing.T) {
	items := make([]int, 100)
	var mu sync.Mutex
	inFlight, peak, done := 0, 0, 0
	forEachBounded(items, 8, func(_ int, _ int) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		inFlight--
		done++
		mu.Unlock()
	})
	if done != 100 {
		t.Errorf("done = %d, want 100", done)
	}
	if peak > 8 {
		t.Errorf("peak concurrency = %d, want <= 8", peak)
	}
}
