package library

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// LayerBundle is an ordered, deduplicated set of materialized layer
// markdown fragments ready to be included in an LLM prompt.
type LayerBundle struct {
	// Layers is the ordered list of layers actually loaded (in the order
	// they were requested), each with the file content already read.
	Layers []LayerContent
}

// LayerContent is a single materialized layer.
type LayerContent struct {
	Name   string
	Source string // root name + relative path, for citations
	Body   string
}

// String concatenates the bundle into a single markdown blob with section
// headers identifying the source of each layer.
func (b *LayerBundle) String() string {
	if b == nil || len(b.Layers) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, l := range b.Layers {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "<!-- layer: %s (%s) -->\n", l.Name, l.Source)
		sb.WriteString(l.Body)
	}
	return sb.String()
}

// IsEmpty reports whether the bundle has any content.
func (b *LayerBundle) IsEmpty() bool {
	return b == nil || len(b.Layers) == 0
}

// Names returns just the ordered layer names. Useful for verbose output and
// run logs.
func (b *LayerBundle) Names() []string {
	if b == nil {
		return nil
	}
	out := make([]string, len(b.Layers))
	for i, l := range b.Layers {
		out[i] = l.Name
	}
	return out
}

// MaterializeLayers reads the named layers from the registry, in the
// requested order, deduplicating by name. Layers that are unavailable
// (filtered out by tool requirements) or whose files cannot be read are
// silently skipped -- they will surface in aida library doctor instead.
func (r *Registry) MaterializeLayers(names []string) *LayerBundle {
	if r == nil {
		return &LayerBundle{}
	}
	bundle := &LayerBundle{}
	seen := make(map[string]bool)
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		layer, ok := r.Layers[name]
		if !ok || !layer.Available {
			continue
		}
		data, err := os.ReadFile(layer.AbsFile)
		if err != nil {
			continue
		}
		src := layer.Root.Ref.Name
		bundle.Layers = append(bundle.Layers, LayerContent{
			Name:   name,
			Source: src,
			Body:   string(data),
		})
	}
	return bundle
}

// MaterializeAllAvailable returns every layer in the registry that is
// available on this machine, in alphabetical order. This is the Step 2
// fallback used before route-driven resolution exists; once routes.yaml is
// implemented, callers should switch to MaterializeLayers with the
// route-resolved name list.
func (r *Registry) MaterializeAllAvailable() *LayerBundle {
	if r == nil {
		return &LayerBundle{}
	}
	names := make([]string, 0, len(r.Layers))
	for name, layer := range r.Layers {
		if layer.Available {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return r.MaterializeLayers(names)
}
