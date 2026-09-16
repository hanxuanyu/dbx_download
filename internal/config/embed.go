package config

import _ "embed"

//go:embed default.yaml
var defaultYAML string

// DefaultYAML returns the commented configuration template shipped with dbxdl.
// It is what "dbxdl config init" writes to disk.
func DefaultYAML() string { return defaultYAML }
