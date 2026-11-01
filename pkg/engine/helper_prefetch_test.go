package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"go.uber.org/zap"
	"net/http"
	"strings"
	"testing"
)

func TestQuadletExplicitHelperImage(t *testing.T) {
	for _, configured := range []string{"example.test/helper:v1", ""} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cached=%t", configured, cached), func(t *testing.T) {
				image := configured
				if image == "" {
					image = fetchitImage
				}
				exists, pulls, creates := 0, 0, 0
				conn := testPodmanConnection(t, func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.Contains(r.URL.Path, "/images/"+image+"/exists"):
						exists++
						if cached {
							w.WriteHeader(204)
						} else {
							w.WriteHeader(404)
						}
					case strings.HasSuffix(r.URL.Path, "/images/pull"):
						pulls++
						if r.URL.Query().Get("reference") != image {
							t.Errorf("wrong pull: %s", r.URL.RawQuery)
						}
						fmt.Fprint(w, `{"images":["test-id"]}`)
					case strings.HasSuffix(r.URL.Path, "/containers/create"):
						creates++
						var spec struct {
							Image string `json:"image"`
						}
						if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
							t.Error(err)
						}
						if spec.Image != image {
							t.Errorf("created helper %q, want %q", spec.Image, image)
						}
						// Stop at creation: no container or host work is performed by this API fixture.
						w.WriteHeader(500)
						fmt.Fprint(w, `{"message":"intentional stop","response":500}`)
					default:
						t.Errorf("unexpected request: %s", r.URL.Path)
						w.WriteHeader(500)
					}
				})
				q := &Quadlet{HelperImage: configured}
				if err := q.deploy(context.Background(), conn, quadletPlan{}); err == nil {
					t.Fatal("expected fixture stop")
				}
				wantPulls := 1
				if cached {
					wantPulls = 0
				}
				if exists != 1 || pulls != wantPulls || creates != 1 {
					t.Fatalf("exists=%d pulls=%d creates=%d", exists, pulls, creates)
				}
			})
		}
	}
}

func TestPopulatePrefetchOnlyRequiredGenericHelper(t *testing.T) {
	oldFetchit, oldLogger := fetchit, logger
	logger = zap.NewNop().Sugar()
	t.Cleanup(func() { fetchit, logger = oldFetchit, oldLogger })
	for _, tc := range []struct {
		name   string
		config *FetchitConfig
		want   int
	}{
		{"empty", &FetchitConfig{}, 0},
		{"empty admission", &FetchitConfig{TargetConfigs: []*TargetConfig{{Quadlet: []*Quadlet{}}}}, 0},
		{"explicit quadlet", &FetchitConfig{TargetConfigs: []*TargetConfig{{Quadlet: []*Quadlet{{HelperImage: "example.test/helper:v1"}}}}}, 0},
		{"default quadlet is lazy", &FetchitConfig{TargetConfigs: []*TargetConfig{{Quadlet: []*Quadlet{{}}}}}, 0},
		{"filetransfer", &FetchitConfig{TargetConfigs: []*TargetConfig{{FileTransfer: []*FileTransfer{{}}}}}, 1},
		{"systemd", &FetchitConfig{TargetConfigs: []*TargetConfig{{Systemd: []*Systemd{{}}}}}, 1},
		{"disconnected quadlet", &FetchitConfig{TargetConfigs: []*TargetConfig{{Disconnected: true, Device: "/dev/test", Quadlet: []*Quadlet{{HelperImage: "example.test/helper:v1"}}}}}, 1},
		{"device reload", &FetchitConfig{ConfigReload: &ConfigReload{Device: "/dev/test"}}, 1},
		{"device image", &FetchitConfig{Images: []*Image{{ImagePath: "image.tar", Device: "/dev/test"}}}, 1},
		{"http reload", &FetchitConfig{ConfigReload: &ConfigReload{ConfigURL: "https://example.test/config"}}, 0},
		{"raw", &FetchitConfig{TargetConfigs: []*TargetConfig{{Raw: []*Raw{{}}}}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			conn := testPodmanConnection(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/images/"+fetchitImage+"/exists") {
					calls++
					w.WriteHeader(204)
					return
				}
				t.Errorf("unexpected API call: %s", r.URL.Path)
				w.WriteHeader(500)
			})
			fc := &FetchitConfig{conn: conn}
			f := fc.populateFetchit(tc.config)
			defer f.scheduler.Stop()
			if calls != tc.want {
				t.Fatalf("generic helper requests = %d, want %d", calls, tc.want)
			}
			if tc.name == "empty admission" && len(f.methodTargetScheds) != 0 {
				t.Fatal("empty admission scheduled jobs")
			}
		})
	}
}
