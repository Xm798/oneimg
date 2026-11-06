package v1

import (
	"os"
	"slices"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestOpenAPISortEnumsMatchHandlerAllowlists(t *testing.T) {
	raw, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Parameters map[string]struct {
				Name   string `yaml:"name"`
				Schema struct {
					Enum    []string `yaml:"enum"`
					Default string   `yaml:"default"`
				} `yaml:"schema"`
			} `yaml:"parameters"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		parameter   string
		allowlist   map[string]bool
		wantDefault string
	}{
		{parameter: "ImageSort", allowlist: imageSortFields, wantDefault: "created_at"},
		{parameter: "UserSort", allowlist: userSortFields, wantDefault: "id"},
	}
	for _, tt := range tests {
		parameter, ok := document.Components.Parameters[tt.parameter]
		if !ok || parameter.Name != "sort" {
			t.Fatalf("components.parameters.%s missing or not named sort: %+v", tt.parameter, parameter)
		}
		allowed := make([]string, 0, len(tt.allowlist))
		for field := range tt.allowlist {
			allowed = append(allowed, field)
		}
		enum := slices.Clone(parameter.Schema.Enum)
		slices.Sort(allowed)
		slices.Sort(enum)
		if !slices.Equal(enum, allowed) {
			t.Fatalf("%s enum = %v, handler allows %v", tt.parameter, enum, allowed)
		}
		if parameter.Schema.Default != tt.wantDefault || !tt.allowlist[tt.wantDefault] {
			t.Fatalf("%s default = %q, want %q", tt.parameter, parameter.Schema.Default, tt.wantDefault)
		}
	}
}
