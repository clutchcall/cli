package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type projectManifest struct {
	Name          string `json:"name"`
	Lang          string `json:"lang"`
	Endpoint      string `json:"endpoint"`
	SchemaVersion string `json:"schema_version"`
}

// CurrentSchemaVersion is the schema version this CLI was built against.
// In follow-up work this should be wired to apirpc_compiler's generated
// version stamp so `clutch migrate` can diff fields and report breaking changes.
const CurrentSchemaVersion = "0.1.0"

func Run(dir string) error {
	manifestPath := filepath.Join(dir, ".clutchcall.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("no .clutchcall.json found in %s (was this project created with `clutch init`?)", dir)
	}
	var m projectManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("parse %s: %w", manifestPath, err)
	}

	fmt.Printf("Project:        %s (%s)\n", m.Name, m.Lang)
	fmt.Printf("Endpoint:       %s\n", m.Endpoint)
	fmt.Printf("Pinned schema:  %s\n", m.SchemaVersion)
	fmt.Printf("Current schema: %s\n", CurrentSchemaVersion)

	if m.SchemaVersion == CurrentSchemaVersion {
		fmt.Println("Status:         up to date")
		return nil
	}

	// TODO: load apirpc_compiler/schemas/*.json for both versions, diff
	// added/removed/renamed fields, and print a per-method breaking-change
	// report. For now we only flag the version mismatch so users see signal.
	fmt.Println("Status:         schema drift — regenerate bindings (apirpc_compiler) and review CHANGELOG")
	return nil
}
