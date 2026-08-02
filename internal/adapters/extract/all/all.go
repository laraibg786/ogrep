// Package all blank-imports every DocumentExtractor plugin package so
// that a single import of this package wires up the full set of
// supported formats via each plugin's self-registering init().
//
// Currently registered: docx, pptx, xlsx (MS Office formats), jsondoc,
// yamldoc (structured data formats), and text (the plain-text,
// catch-all fallback). Add a new format's blank import here when its
// plugin package is ready.
package all

import (
	_ "github.com/laraibg786/ogrep/internal/adapters/extract/docx"
	_ "github.com/laraibg786/ogrep/internal/adapters/extract/jsondoc"
	_ "github.com/laraibg786/ogrep/internal/adapters/extract/pptx"
	_ "github.com/laraibg786/ogrep/internal/adapters/extract/text"
	_ "github.com/laraibg786/ogrep/internal/adapters/extract/xlsx"
	_ "github.com/laraibg786/ogrep/internal/adapters/extract/yamldoc"
)
