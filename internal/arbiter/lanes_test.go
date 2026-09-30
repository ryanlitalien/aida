package arbiter

import (
	"reflect"
	"testing"
)

func TestLoadExamplesLanesMatchesDefaultConfig(t *testing.T) {
	got, err := Load("../../examples/lanes.yaml")
	if err != nil {
		t.Fatalf("Load(examples/lanes.yaml) error: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Load(examples/lanes.yaml).Validate() error: %v", err)
	}
	want := DefaultConfig()
	if err := want.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("examples/lanes.yaml does not match DefaultConfig()\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/lanes.yaml"); err == nil {
		t.Fatal("Load(missing file) = nil error, want an error")
	}
}

func TestSorted(t *testing.T) {
	cfg := &Config{Lanes: []Lane{
		{ID: "c", Order: 3},
		{ID: "a", Order: 1},
		{ID: "b", Order: 2},
	}}
	sorted := cfg.Sorted()
	var ids []string
	for _, l := range sorted {
		ids = append(ids, l.ID)
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("Sorted() ids = %v, want %v", ids, want)
	}
}

func TestSortedDoesNotMutateOriginal(t *testing.T) {
	cfg := &Config{Lanes: []Lane{
		{ID: "c", Order: 3},
		{ID: "a", Order: 1},
	}}
	_ = cfg.Sorted()
	if cfg.Lanes[0].ID != "c" {
		t.Fatalf("Sorted() mutated the original slice: %+v", cfg.Lanes)
	}
}

func TestEffectiveMinHeadroomPct(t *testing.T) {
	if got := (Lane{}).EffectiveMinHeadroomPct(); got != defaultMinHeadroomPct {
		t.Errorf("zero-value lane EffectiveMinHeadroomPct() = %v, want %v", got, defaultMinHeadroomPct)
	}
	if got := (Lane{MinHeadroomPct: 12}).EffectiveMinHeadroomPct(); got != 12 {
		t.Errorf("EffectiveMinHeadroomPct() = %v, want 12", got)
	}
}

func TestEffectiveHandoffAtPct(t *testing.T) {
	if got := (Lane{}).EffectiveHandoffAtPct(); got != defaultHandoffAtPct {
		t.Errorf("zero-value lane EffectiveHandoffAtPct() = %v, want %v", got, defaultHandoffAtPct)
	}
	if got := (Lane{HandoffAtPct: 75}).EffectiveHandoffAtPct(); got != 75 {
		t.Errorf("EffectiveHandoffAtPct() = %v, want 75", got)
	}
}

func TestLaneServesRole(t *testing.T) {
	all := Lane{}
	if !all.ServesRole("executor") || !all.ServesRole("thinker") {
		t.Error("empty Roles should serve every role")
	}
	thinkerOnly := Lane{Roles: []string{RoleThinker}}
	if thinkerOnly.ServesRole(RoleExecutor) {
		t.Error("thinker-only lane should not serve executor")
	}
	if !thinkerOnly.ServesRole(RoleThinker) {
		t.Error("thinker-only lane should serve thinker")
	}
}

func TestLaneAllowsClass(t *testing.T) {
	l := Lane{DataClasses: []DataClass{DataClassPersonal, DataClassPublic}}
	if !l.AllowsClass(DataClassPersonal) {
		t.Error("expected personal to be allowed")
	}
	if l.AllowsClass(DataClassButterstack) {
		t.Error("expected butterstack to be disallowed")
	}
}

func TestLaneModelFor(t *testing.T) {
	l := Lane{Models: map[string]string{RoleExecutor: "sonnet", RoleThinker: "opus"}}
	if got := l.ModelFor(RoleThinker); got != "opus" {
		t.Errorf("ModelFor(thinker) = %q, want opus", got)
	}
	if got := l.ModelFor(RoleTrivial); got != "sonnet" {
		t.Errorf("ModelFor(trivial) falls back to executor = %q, want sonnet", got)
	}
}

func baseValidLane() Lane {
	return Lane{
		ID:          "x",
		Order:       1,
		DataClasses: []DataClass{DataClassPersonal},
		Runner:      RunnerClaude,
		Auth:        AuthSubscription,
		Models:      map[string]string{RoleExecutor: "m"},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func([]Lane) []Lane
		wantErr bool
	}{
		{
			name:    "valid single lane",
			mutate:  func(l []Lane) []Lane { return l },
			wantErr: false,
		},
		{
			name: "duplicate id",
			mutate: func(l []Lane) []Lane {
				dup := l[0]
				dup.Order = 2
				return append(l, dup)
			},
			wantErr: true,
		},
		{
			name: "duplicate order",
			mutate: func(l []Lane) []Lane {
				dup := l[0]
				dup.ID = "y"
				return append(l, dup)
			},
			wantErr: true,
		},
		{
			name: "empty data classes",
			mutate: func(l []Lane) []Lane {
				l[0].DataClasses = nil
				return l
			},
			wantErr: true,
		},
		{
			name: "fable as the executor model (task #481)",
			mutate: func(l []Lane) []Lane {
				l[0].Models[RoleExecutor] = "claude-fable-5-1"
				return l
			},
			wantErr: true,
		},
		{
			name: "unknown runner",
			mutate: func(l []Lane) []Lane {
				l[0].Runner = "carrier-pigeon"
				return l
			},
			wantErr: true,
		},
		{
			name: "exec runner without command",
			mutate: func(l []Lane) []Lane {
				l[0].Runner = RunnerExec
				l[0].Command = nil
				return l
			},
			wantErr: true,
		},
		{
			name: "missing models.executor",
			mutate: func(l []Lane) []Lane {
				l[0].Models = map[string]string{RoleThinker: "m"}
				return l
			},
			wantErr: true,
		},
		{
			name: "personal and butterstack together",
			mutate: func(l []Lane) []Lane {
				l[0].DataClasses = []DataClass{DataClassPersonal, DataClassButterstack}
				return l
			},
			wantErr: true,
		},
		{
			name: "subscription lane with exec runner is fine",
			mutate: func(l []Lane) []Lane {
				l[0].Runner = RunnerExec
				l[0].Command = []string{"agy", "{prompt}"}
				return l
			},
			wantErr: false,
		},
		{
			name: "subscription lane with aida-agent runner",
			mutate: func(l []Lane) []Lane {
				l[0].Runner = RunnerAidaAgent
				return l
			},
			wantErr: true,
		},
		{
			name: "subscription lane sets ANTHROPIC_BASE_URL",
			mutate: func(l []Lane) []Lane {
				l[0].Env = map[string]string{"ANTHROPIC_BASE_URL": "http://evil"}
				return l
			},
			wantErr: true,
		},
		{
			name: "subscription lane sets ANTHROPIC_API_KEY",
			mutate: func(l []Lane) []Lane {
				l[0].Env = map[string]string{"ANTHROPIC_API_KEY": "sk-x"}
				return l
			},
			wantErr: true,
		},
		{
			name: "subscription lane sets ANTHROPIC_AUTH_TOKEN",
			mutate: func(l []Lane) []Lane {
				l[0].Env = map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok"}
				return l
			},
			wantErr: true,
		},
		{
			name: "api-key lane sets config_dir",
			mutate: func(l []Lane) []Lane {
				l[0].Auth = AuthAPIKey
				l[0].ConfigDir = "~/.claude-x"
				return l
			},
			wantErr: true,
		},
		{
			name: "api-key lane without config_dir is fine",
			mutate: func(l []Lane) []Lane {
				l[0].Auth = AuthAPIKey
				return l
			},
			wantErr: false,
		},
		{
			name: "unknown auth",
			mutate: func(l []Lane) []Lane {
				l[0].Auth = "carrier-pigeon"
				return l
			},
			wantErr: true,
		},
		{
			name: "empty id",
			mutate: func(l []Lane) []Lane {
				l[0].ID = ""
				return l
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Lanes: tt.mutate([]Lane{baseValidLane()})}
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateNilConfig(t *testing.T) {
	var cfg *Config
	if err := cfg.Validate(); err != nil {
		t.Errorf("nil Config.Validate() = %v, want nil", err)
	}
}

func TestDefaultConfigValidates(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() error: %v", err)
	}
}

func TestDefaultConfigOrderIsCheapestFirst(t *testing.T) {
	want := []string{"claude-personal", "claude-company", "gemini", "codex", "proxy-model", "litellm"}
	sorted := DefaultConfig().Sorted()
	if len(sorted) != len(want) {
		t.Fatalf("got %d lanes, want %d", len(sorted), len(want))
	}
	for i, id := range want {
		if sorted[i].ID != id {
			t.Errorf("lane %d = %q, want %q", i, sorted[i].ID, id)
		}
	}
}
