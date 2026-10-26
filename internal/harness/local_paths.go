package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fullsend-ai/fullsend/internal/skill"
)

type pathResolver struct {
	harnessRel      string
	harnessDir      string
	containmentRoot string
	fallback        bool
	root            *os.Root
	diagnostics     []Diagnostic
}

func newPathResolver(harnessDir, containmentRoot string, fallback bool) (*pathResolver, error) {
	if harnessDir == "" {
		return nil, fmt.Errorf("harness directory is required for path resolution")
	}
	if containmentRoot == "" {
		containmentRoot = harnessDir
	}

	absHarnessDir, err := filepath.Abs(harnessDir)
	if err != nil {
		return nil, fmt.Errorf("resolving harness directory: %w", err)
	}
	absContainmentRoot, err := filepath.Abs(containmentRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving containment directory: %w", err)
	}

	canonicalHarnessDir, err := filepath.EvalSymlinks(absHarnessDir)
	if err != nil {
		canonicalHarnessDir = filepath.Clean(absHarnessDir)
	}
	canonicalContainmentRoot, err := filepath.EvalSymlinks(absContainmentRoot)
	if err != nil {
		canonicalContainmentRoot = filepath.Clean(absContainmentRoot)
	}
	harnessRel, err := filepath.Rel(canonicalContainmentRoot, canonicalHarnessDir)
	if err != nil {
		return nil, fmt.Errorf("resolving harness directory relative to containment directory: %w", err)
	}
	if harnessRel == ".." || strings.HasPrefix(harnessRel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("harness directory %q is outside fullsend directory %q", harnessDir, containmentRoot)
	}

	resolver := &pathResolver{
		harnessRel:      harnessRel,
		harnessDir:      filepath.Clean(absHarnessDir),
		containmentRoot: filepath.Clean(absContainmentRoot),
		fallback:        fallback,
	}
	if root, err := os.OpenRoot(resolver.containmentRoot); err == nil {
		resolver.root = root
	}
	return resolver, nil
}

func (r *pathResolver) Close() {
	if r.root != nil {
		_ = r.root.Close()
	}
}

func (r *pathResolver) resolve(field, path string) (string, error) {
	if path == "" || filepath.IsAbs(path) || IsURL(path) {
		return path, nil
	}

	resolved := filepath.Clean(filepath.Join(r.containmentRoot, r.harnessRel, path))
	if !r.withinContainmentRoot(resolved) {
		return "", fmt.Errorf("%s: path %q resolves outside fullsend directory", field, path)
	}

	if r.fallback {
		legacyResolved := filepath.Clean(filepath.Join(r.containmentRoot, path))
		legacyWithinRoot := r.withinContainmentRoot(legacyResolved)
		if legacyWithinRoot && legacyResolved != resolved &&
			localPathExists(legacyResolved) && !localPathExists(resolved) {
			r.diagnostics = append(r.diagnostics, Diagnostic{
				Severity: SeverityWarning,
				Field:    field,
				Message: fmt.Sprintf(
					"path %q in harness directory %q uses the deprecated --fullsend-dir-relative fallback %q; make it relative to the harness file (the fallback will be removed)",
					path, r.harnessDir, legacyResolved,
				),
			})
			return legacyResolved, nil
		}
	}

	return resolved, nil
}

func (r *pathResolver) resolveString(field string, value *string) error {
	resolved, err := r.resolve(field, *value)
	if err != nil {
		return err
	}
	*value = resolved
	return nil
}

func (r *pathResolver) resolveSkill(field string, entry *SkillEntry) error {
	original := entry.Source
	resolved, err := r.resolve(field, original)
	if err != nil {
		return err
	}
	entry.Source = resolved
	if original == "" || filepath.IsAbs(original) || IsURL(original) {
		return nil
	}

	relativeToRoot, relErr := filepath.Rel(r.containmentRoot, resolved)
	if relErr != nil || !markersWithin(r.root, relativeToRoot) {
		entry.mergeName = filepath.Base(filepath.Clean(original))
	} else {
		entry.mergeName = skill.SandboxName(resolved)
	}
	return nil
}

func (r *pathResolver) resolveHarness(h *Harness) error {
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"agent", &h.Agent},
		{"policy", &h.Policy},
		{"pre_script", &h.PreScript},
		{"post_script", &h.PostScript},
		{"agent_input", &h.AgentInput},
	} {
		if err := r.resolveString(field.name, field.value); err != nil {
			return err
		}
	}

	for i := range h.Skills {
		field := fmt.Sprintf("skills[%d]", i)
		if err := r.resolveSkill(field, &h.Skills[i]); err != nil {
			return err
		}
		for key, value := range h.Skills[i].Overrides {
			if value == nil {
				continue
			}
			if err := r.resolveString(fmt.Sprintf("%s.overrides[%s]", field, key), value); err != nil {
				return err
			}
		}
	}

	for i := range h.Plugins {
		if err := r.resolveString(fmt.Sprintf("plugins[%d]", i), &h.Plugins[i].Path); err != nil {
			return err
		}
	}
	for i := range h.HostFiles {
		if strings.Contains(h.HostFiles[i].Src, "${") {
			continue
		}
		if err := r.resolveString(fmt.Sprintf("host_files[%d].src", i), &h.HostFiles[i].Src); err != nil {
			return err
		}
	}
	for i := range h.APIServers {
		if err := r.resolveString(fmt.Sprintf("api_servers[%d].script", i), &h.APIServers[i].Script); err != nil {
			return err
		}
	}
	if h.ValidationLoop != nil {
		if err := r.resolveString("validation_loop.script", &h.ValidationLoop.Script); err != nil {
			return err
		}
		if h.ValidationLoop.Schema != "" && !strings.Contains(h.ValidationLoop.Schema, "${") {
			if err := r.resolveString("validation_loop.schema", &h.ValidationLoop.Schema); err != nil {
				return err
			}
		}
	}
	if h.OpenShell != nil {
		for i := range h.OpenShell.Profiles {
			if err := r.resolveString(fmt.Sprintf("openshell.profiles[%d]", i), &h.OpenShell.Profiles[i]); err != nil {
				return err
			}
		}
	}
	for i := range h.Providers {
		if !IsProviderPath(h.Providers[i]) {
			continue
		}
		if err := r.resolveString(fmt.Sprintf("providers[%d]", i), &h.Providers[i]); err != nil {
			return err
		}
	}

	for platform, config := range h.Forge {
		if config == nil {
			continue
		}
		if err := r.resolveForgeConfig(fmt.Sprintf("forge.%s", platform), config); err != nil {
			return err
		}
	}
	for i := range h.Overlays {
		if err := r.resolveForgeConfig(fmt.Sprintf("overlays[%d]", i), &h.Overlays[i].ForgeConfig); err != nil {
			return err
		}
	}
	return nil
}

func (r *pathResolver) resolveForgeConfig(prefix string, config *ForgeConfig) error {
	if config == nil {
		return nil
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{prefix + ".pre_script", &config.PreScript},
		{prefix + ".post_script", &config.PostScript},
		{prefix + ".policy", &config.Policy},
	} {
		if err := r.resolveString(field.name, field.value); err != nil {
			return err
		}
	}
	for i := range config.Skills {
		field := fmt.Sprintf("%s.skills[%d]", prefix, i)
		if err := r.resolveSkill(field, &config.Skills[i]); err != nil {
			return err
		}
		for key, value := range config.Skills[i].Overrides {
			if value == nil {
				continue
			}
			if err := r.resolveString(fmt.Sprintf("%s.overrides[%s]", field, key), value); err != nil {
				return err
			}
		}
	}
	for i := range config.HostFiles {
		if strings.Contains(config.HostFiles[i].Src, "${") {
			continue
		}
		if err := r.resolveString(fmt.Sprintf("%s.host_files[%d].src", prefix, i), &config.HostFiles[i].Src); err != nil {
			return err
		}
	}
	if config.ValidationLoop != nil {
		if err := r.resolveString(prefix+".validation_loop.script", &config.ValidationLoop.Script); err != nil {
			return err
		}
		if config.ValidationLoop.Schema != "" && !strings.Contains(config.ValidationLoop.Schema, "${") {
			if err := r.resolveString(prefix+".validation_loop.schema", &config.ValidationLoop.Schema); err != nil {
				return err
			}
		}
	}
	if config.OpenShell != nil {
		for i := range config.OpenShell.Profiles {
			if err := r.resolveString(fmt.Sprintf("%s.openshell.profiles[%d]", prefix, i), &config.OpenShell.Profiles[i]); err != nil {
				return err
			}
		}
	}
	for i := range config.Providers {
		if !IsProviderPath(config.Providers[i]) {
			continue
		}
		if err := r.resolveString(fmt.Sprintf("%s.providers[%d]", prefix, i), &config.Providers[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *pathResolver) withinContainmentRoot(path string) bool {
	relative, err := filepath.Rel(r.containmentRoot, path)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func localPathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func harnessDirWithinContainmentRoot(harnessDir, containmentRoot string) bool {
	absHarnessDir, err := filepath.Abs(harnessDir)
	if err != nil {
		return false
	}
	if canonical, evalErr := filepath.EvalSymlinks(absHarnessDir); evalErr == nil {
		absHarnessDir = canonical
	}
	absContainmentRoot, err := filepath.Abs(containmentRoot)
	if err != nil {
		return false
	}
	if canonical, evalErr := filepath.EvalSymlinks(absContainmentRoot); evalErr == nil {
		absContainmentRoot = canonical
	}

	relative, err := filepath.Rel(absContainmentRoot, absHarnessDir)
	return err == nil && (relative == "." ||
		(relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func (h *Harness) resolveLocalPathsRelativeTo(harnessDir, containmentRoot string) error {
	resolver, err := newPathResolver(harnessDir, containmentRoot, true)
	if err != nil {
		return err
	}
	defer resolver.Close()

	if err := resolver.resolveHarness(h); err != nil {
		return err
	}
	h.pathDiagnostics = append(h.pathDiagnostics, resolver.diagnostics...)
	return nil
}
