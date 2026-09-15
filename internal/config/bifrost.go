package config

// BifrostPreset is one operator-configured "start a herdr agent like
// this" recipe, read from config.yaml's `bifrost.presets` block.
//
// This mirrors internal/bifrost.Preset field-for-field. It is declared
// here rather than reused from internal/bifrost because internal/config
// must not import internal/bifrost -- config is a low-level package read
// by nearly everything (including, transitively, bifrost's own callers),
// so importing bifrost from here would point the dependency arrow the
// wrong way. The cli-layer call site (serve.go, which wires up the
// bifrost.Client) converts a []BifrostPreset into a []bifrost.Preset when
// constructing the client.
type BifrostPreset struct {
	Name string `yaml:"name" json:"name"`
	// User selects which /bifrost lane (see FleetConfig.BifrostLanes) this
	// preset belongs to. Empty means "the first configured lane" -- the
	// cli-layer call site resolves that fallback and fills User in before
	// handing the preset to a bifrost.Client, so a preset read back off a
	// Client never has an empty User even if config.yaml left it unset.
	User string   `yaml:"user,omitempty" json:"user,omitempty"`
	Cwd  string   `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Argv []string `yaml:"argv" json:"argv"`
}

// BifrostConfig holds settings for the /bifrost dashboard page.
type BifrostConfig struct {
	// Presets are the agent-launch recipes offered by the page's "start
	// session" control. Empty means no launchable presets -- the page
	// still shows read/send against agents herdr already knows about.
	Presets []BifrostPreset `yaml:"presets,omitempty"`
}
