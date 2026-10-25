package clihelp

import "testing"

func TestParseCatalogYAML_RejectsDuplicatePath(t *testing.T) {
	data := []byte(`
program: go-ios
usage: ["ios [--help]"]
commands:
  - path: apps
    usage: ios apps [options]
    summary: one
  - path: apps
    usage: ios apps [options]
    summary: two
`)
	_, err := parseCatalogYAML(data)
	if err == nil {
		t.Fatal("expected duplicate command path error")
	}
}

func TestParseCatalogYAML_RejectsMissingUsage(t *testing.T) {
	data := []byte(`
program: go-ios
usage: ["ios [--help]"]
commands:
  - path: apps
    summary: app list
`)
	_, err := parseCatalogYAML(data)
	if err == nil {
		t.Fatal("expected missing usage error")
	}
}
