// Package examples embeds the fictional worked files under examples/ (see
// examples/README.md) as an embed.FS, so `aida init` can copy them into a
// fresh ~/.aida/ from a plain `go install`'d binary -- one that has no
// source checkout on disk at runtime -- instead of only working from
// inside this repo.
package examples

import "embed"

//go:embed sources/*.yaml roster.yaml env.example op.env.example golden/questions.txt golden/expectations.yaml
var FS embed.FS
