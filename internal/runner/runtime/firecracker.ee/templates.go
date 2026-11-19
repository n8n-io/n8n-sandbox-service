package firecracker

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// templateAssets locates one template's guest assets: a template directory
// holding rootfs.ext4 and vmlinux, and its golden snapshot. The default
// template's come from SANDBOX_RUNNER_FIRECRACKER_TEMPLATE_DIR and the
// SNAPSHOT_*_PATH settings; others from SANDBOX_RUNNER_FIRECRACKER_TEMPLATES.
type templateAssets struct {
	Name              string `json:"-"`
	TemplateDir       string `json:"template_dir"`
	SnapshotMemPath   string `json:"snapshot_mem_path"`
	SnapshotStatePath string `json:"snapshot_state_path"`
}

func (a templateAssets) rootfsPath() string { return filepath.Join(a.TemplateDir, "rootfs.ext4") }
func (a templateAssets) kernelPath() string { return filepath.Join(a.TemplateDir, "vmlinux") }

// bootParamsPath is the sidecar in the golden snapshot's directory.
func (a templateAssets) bootParamsPath() string {
	return filepath.Join(filepath.Dir(a.SnapshotMemPath), bootParamsFileName)
}

// defaultTemplateAssets are the default template's assets.
func (c Config) defaultTemplateAssets() templateAssets {
	return templateAssets{
		Name:              runnerruntime.DefaultTemplate,
		TemplateDir:       c.TemplateDir,
		SnapshotMemPath:   c.SnapshotMemPath,
		SnapshotStatePath: c.SnapshotStatePath,
	}
}

// templateAssetsList returns every template's assets, the default first and
// the rest by name.
func (c Config) templateAssetsList() []templateAssets {
	out := []templateAssets{c.defaultTemplateAssets()}
	names := make([]string, 0, len(c.Templates))
	for name := range c.Templates {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		a := c.Templates[name]
		a.Name = name
		out = append(out, a)
	}
	return out
}

// templateAssetsFor returns the assets of name ("" is the default).
func (c Config) templateAssetsFor(name string) (templateAssets, bool) {
	if name == "" || name == runnerruntime.DefaultTemplate {
		return c.defaultTemplateAssets(), true
	}
	a, ok := c.Templates[name]
	a.Name = name
	return a, ok
}

// parseTemplates decodes SANDBOX_RUNNER_FIRECRACKER_TEMPLATES strictly.
func parseTemplates(v string) (map[string]templateAssets, error) {
	dec := json.NewDecoder(strings.NewReader(v))
	dec.DisallowUnknownFields()
	var out map[string]templateAssets
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data")
	}
	if _, ok := out[runnerruntime.DefaultTemplate]; ok {
		return nil, fmt.Errorf("%q is set by SANDBOX_RUNNER_FIRECRACKER_TEMPLATE_DIR and the SNAPSHOT_*_PATH settings", runnerruntime.DefaultTemplate)
	}
	return out, nil
}

// validateTemplates checks the extra templates as validateConfig checks the
// default's paths. Two templates sharing a snapshot directory would overwrite
// each other's boot.json, so that is refused.
func validateTemplates(cfg Config) error {
	snapshotDirs := map[string]string{}
	for _, a := range cfg.templateAssetsList() {
		if a.Name != runnerruntime.DefaultTemplate {
			if err := runnerruntime.ValidateTemplateName(a.Name); err != nil {
				return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_TEMPLATES: %w", err)
			}
			for field, value := range map[string]string{
				"template_dir": a.TemplateDir, "snapshot_mem_path": a.SnapshotMemPath, "snapshot_state_path": a.SnapshotStatePath,
			} {
				if !strings.HasPrefix(value, "/") {
					return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_TEMPLATES: template %q: %s must be an absolute path, got %q", a.Name, field, value)
				}
			}
			if cfg.CreateSnapshotScript != "" && !snapshotDirsMatch(a.SnapshotMemPath, a.SnapshotStatePath) {
				return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_TEMPLATES: template %q: snapshot_mem_path and snapshot_state_path must be in the same directory when SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT is set", a.Name)
			}
		}
		dir := filepath.Clean(filepath.Dir(a.SnapshotMemPath))
		if other, ok := snapshotDirs[dir]; ok {
			return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_TEMPLATES: templates %q and %q share the snapshot directory %s", other, a.Name, dir)
		}
		snapshotDirs[dir] = a.Name
	}
	return nil
}
