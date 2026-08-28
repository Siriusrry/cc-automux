package version

import (
	_ "embed"
	"strings"
)

const (
	ProductName = "CC AutoMux"
	BinaryName  = "cc-automux"
)

//go:embed VERSION
var embedded string

// Current returns the canonical version embedded from VERSION.
func Current() string {
	return strings.TrimSpace(embedded)
}

// Display returns the complete user-facing product and version string.
func Display() string {
	return ProductName + " " + Current()
}
