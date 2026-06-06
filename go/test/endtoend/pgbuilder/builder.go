// Copyright 2026 Supabase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package pgbuilder provides reusable helpers for checking out, building, and
// installing a pinned PostgreSQL source tree from git for end-to-end tests.
//
// Both the PostgreSQL regression/isolation harness (pgregresstest) and the
// sqllogictest differential harness (queryserving/sqllogictest) consume this
// package so they run against the same PostgreSQL version.
package pgbuilder

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigres/multigres/go/tools/executil"
)

const (
	// PostgresGitRepo is the official PostgreSQL git repository.
	PostgresGitRepo = "https://github.com/postgres/postgres"

	// PostgresVersion is the git tag to checkout.
	PostgresVersion = "REL_17_6"

	// PostgresCacheDir is the default cache directory for PostgreSQL source and builds.
	PostgresCacheDir = "/tmp/multigres_pg_cache"
)

// Builder manages a PostgreSQL source checkout, an isolated build, and a
// per-test install prefix. It does not run any server processes itself;
// callers either invoke the built binaries directly or compose Builder with
// higher-level helpers (see Standalone in this package).
type Builder struct {
	// SourceDir is the shared source checkout, reused across runs.
	SourceDir string
	// BuildDir is the per-invocation build directory.
	BuildDir string
	// InstallDir is the per-invocation install prefix (contains bin/, lib/, share/).
	InstallDir string
	// ExternalDir is the per-invocation root under which external extensions
	// (KindExternal, e.g. pgvector) are cloned and built as PGXS modules. It is
	// per-run (under the same build root as BuildDir) so concurrent callers
	// don't share a checkout, and is removed by Cleanup along with the build.
	ExternalDir string
	// OutputDir is a persistent per-invocation directory for caller-written artifacts
	// (reports, diffs, etc.). pgbuilder itself does not write here.
	OutputDir string
	// ConfigureArgs are extra flags appended to ./configure. Callers set this
	// before Build to enable optional features that some contrib extensions
	// require (e.g. --with-uuid for uuid-ossp). Empty by default so existing
	// callers (regression/isolation, sqllogictest) build unchanged.
	ConfigureArgs []string
}

// New returns a Builder with unique per-invocation build and install directories
// rooted at $MULTIGRES_PG_CACHE_DIR (or /tmp/multigres_pg_cache when unset).
// Multiple concurrent callers get distinct build/install trees but share the
// source checkout.
func New(t *testing.T) *Builder {
	t.Helper()

	cacheDir := os.Getenv("MULTIGRES_PG_CACHE_DIR")
	if cacheDir == "" {
		cacheDir = PostgresCacheDir
	}

	timestamp := time.Now().Format("20060102-150405.000000")
	buildRoot := filepath.Join(cacheDir, "builds", timestamp)

	return &Builder{
		SourceDir:   filepath.Join(cacheDir, "source", "postgres"),
		BuildDir:    filepath.Join(buildRoot, "build"),
		InstallDir:  filepath.Join(buildRoot, "install"),
		ExternalDir: filepath.Join(buildRoot, "external"),
		OutputDir:   filepath.Join(cacheDir, "results", timestamp),
	}
}

// BinDir is the directory containing the built PostgreSQL binaries (postgres,
// initdb, psql, pg_ctl, ...).
func (b *Builder) BinDir() string {
	return filepath.Join(b.InstallDir, "bin")
}

// CheckBuildDependencies verifies that required C toolchain is available on
// the host. Callers that depend on building PostgreSQL from source should
// invoke this early and skip the test on a clear error message.
func CheckBuildDependencies(t *testing.T) error {
	t.Helper()

	required := []string{"make", "gcc"}
	var missing []string
	for _, tool := range required {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing build dependencies: %v. Install with: apt-get install build-essential", missing)
	}
	return nil
}

// EnsureSource ensures the pinned PostgreSQL source tree is available,
// cloning it if missing or wrong version. The source is shared across
// concurrent builders.
func (b *Builder) EnsureSource(t *testing.T, ctx context.Context) error {
	t.Helper()

	if _, err := os.Stat(b.SourceDir); err == nil {
		t.Logf("Found cached PostgreSQL source at %s, verifying version...", b.SourceDir)

		cmd := executil.Command(ctx, "git", "-C", b.SourceDir, "describe", "--tags", "--exact-match")
		output, err := cmd.Output()
		if err == nil && strings.TrimSpace(string(output)) == PostgresVersion {
			t.Logf("Using cached PostgreSQL source (version %s)", PostgresVersion)
			return nil
		}

		t.Logf("Cached source version mismatch or invalid, re-cloning...")
		if err := os.RemoveAll(b.SourceDir); err != nil {
			return fmt.Errorf("failed to remove old cache: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(b.SourceDir), 0o755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	t.Logf("Cloning PostgreSQL %s from %s...", PostgresVersion, PostgresGitRepo)
	cmd := executil.Command(ctx, "git", "clone",
		"--depth=1",
		"--branch", PostgresVersion,
		PostgresGitRepo,
		b.SourceDir)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to clone PostgreSQL: %w (stderr: %s)", err, stderr.String())
	}

	t.Logf("Successfully cloned PostgreSQL %s", PostgresVersion)
	return nil
}

// Build runs ./configure + make + make install into b.InstallDir.
// ICU is disabled so the build does not require icu4c headers; that matches
// what the existing pgregresstest suite already does.
func (b *Builder) Build(t *testing.T, ctx context.Context) error {
	t.Helper()

	if err := os.MkdirAll(b.BuildDir, 0o755); err != nil {
		return fmt.Errorf("failed to create build directory: %w", err)
	}

	t.Logf("Configuring PostgreSQL with ./configure...")
	configureArgs := []string{
		"--prefix=" + b.InstallDir,
		"--enable-cassert=no",
		"--enable-tap-tests=no",
		"--without-icu",
	}
	configureArgs = append(configureArgs, b.ConfigureArgs...)
	configureCmd := executil.Command(ctx, filepath.Join(b.SourceDir, "configure"), configureArgs...)
	configureCmd.Dir = b.BuildDir
	configureCmd.Stdout = os.Stdout
	configureCmd.Stderr = os.Stderr
	if err := configureCmd.Run(); err != nil {
		return fmt.Errorf("configure failed: %w", err)
	}

	t.Logf("Building PostgreSQL with make...")
	makeCmd := executil.Command(ctx, "make", "-j", "4")
	makeCmd.Dir = b.BuildDir
	makeCmd.Stdout = os.Stdout
	makeCmd.Stderr = os.Stderr
	if err := makeCmd.Run(); err != nil {
		return fmt.Errorf("make failed: %w", err)
	}

	t.Logf("Installing PostgreSQL to %s...", b.InstallDir)
	installCmd := executil.Command(ctx, "make", "install")
	installCmd.Dir = b.BuildDir
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("make install failed: %w", err)
	}

	t.Logf("PostgreSQL build completed successfully")
	return nil
}

// InstallContrib builds and installs all configured contrib modules into
// b.InstallDir. Must be called after Build().
//
// The top-level `make install` run by Build installs only the core server;
// contrib extensions (citext, hstore, cube, ...) ship their own
// .so/.control/.sql and must be installed separately before CREATE EXTENSION
// can load them. contrib/Makefile already skips modules whose optional
// dependencies (libxml, openssl, ...) were not enabled at configure time, so
// this installs only what the --without-icu configure produced.
func (b *Builder) InstallContrib(t *testing.T, ctx context.Context) error {
	t.Helper()

	contribDir := filepath.Join(b.BuildDir, "contrib")
	t.Logf("Building and installing contrib modules from %s...", contribDir)

	cmd := executil.Command(ctx, "make", "-C", contribDir, "-j", "4", "install")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("make -C contrib install failed: %w", err)
	}

	t.Logf("contrib modules installed to %s", b.InstallDir)
	return nil
}

// ExtBuildSystem selects how an external extension's source tree is compiled.
type ExtBuildSystem int

const (
	// ExtBuildPGXS: `make PG_CONFIG=...` then `make install` (e.g. pgvector). Zero value.
	ExtBuildPGXS ExtBuildSystem = iota
	// ExtBuildAutotools: Bootstrap, then ./configure --with-pgconfig, make, make install (e.g. PostGIS).
	ExtBuildAutotools
)

// ExtSpec describes how to fetch and build one external extension.
type ExtSpec struct {
	Name string
	Repo string
	Tag  string // pinned for reproducibility
	// Build selects the build system; zero value is ExtBuildPGXS.
	Build ExtBuildSystem
	// Bootstrap runs in the checkout before ./configure (autotools only), e.g. {{"./autogen.sh"}}.
	Bootstrap [][]string
	// ConfigureArgs are extra ./configure flags (autotools only); --with-pgconfig is added automatically.
	ConfigureArgs []string
}

// InstallExternalExtension clones spec.Repo@spec.Tag into b.ExternalDir, builds
// it against this builder's from-source PostgreSQL (PGXS or autotools per
// spec.Build) so the .so matches the cluster's ABI, installs it, and returns the
// checkout dir. Must be called after Build().
func (b *Builder) InstallExternalExtension(t *testing.T, ctx context.Context, spec ExtSpec) (string, error) {
	t.Helper()

	if err := os.MkdirAll(b.ExternalDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create external dir: %w", err)
	}
	cloneDir := filepath.Join(b.ExternalDir, spec.Name)

	t.Logf("Cloning external extension %s (%s) from %s...", spec.Name, spec.Tag, spec.Repo)
	clone := executil.Command(ctx, "git", "clone",
		"--depth=1",
		"--branch", spec.Tag,
		spec.Repo,
		cloneDir)
	var stderr bytes.Buffer
	clone.Stderr = &stderr
	if err := clone.Run(); err != nil {
		return "", fmt.Errorf("failed to clone %s: %w (stderr: %s)", spec.Name, err, stderr.String())
	}

	pgConfig := filepath.Join(b.BinDir(), "pg_config")

	switch spec.Build {
	case ExtBuildAutotools:
		if err := b.buildExternalAutotools(t, ctx, spec, cloneDir, pgConfig); err != nil {
			return "", err
		}
	default: // ExtBuildPGXS
		if err := b.buildExternalPGXS(t, ctx, spec, cloneDir, pgConfig); err != nil {
			return "", err
		}
	}

	t.Logf("External extension %s installed", spec.Name)
	return cloneDir, nil
}

// buildExternalPGXS builds and installs a PGXS module (make PG_CONFIG=...; make install).
func (b *Builder) buildExternalPGXS(t *testing.T, ctx context.Context, spec ExtSpec, cloneDir, pgConfig string) error {
	t.Helper()

	t.Logf("Building external extension %s with PGXS (PG_CONFIG=%s)...", spec.Name, pgConfig)
	makeCmd := executil.Command(ctx, "make", "-C", cloneDir, "-j", "4", "PG_CONFIG="+pgConfig)
	makeCmd.Stdout = os.Stdout
	makeCmd.Stderr = os.Stderr
	if err := makeCmd.Run(); err != nil {
		return fmt.Errorf("make %s failed: %w", spec.Name, err)
	}

	t.Logf("Installing external extension %s into %s...", spec.Name, b.InstallDir)
	installCmd := executil.Command(ctx, "make", "-C", cloneDir, "PG_CONFIG="+pgConfig, "install")
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("make %s install failed: %w", spec.Name, err)
	}
	return nil
}

// buildExternalAutotools runs Bootstrap, then ./configure --with-pgconfig, make, make install (e.g. PostGIS).
func (b *Builder) buildExternalAutotools(t *testing.T, ctx context.Context, spec ExtSpec, cloneDir, pgConfig string) error {
	t.Helper()

	for _, argv := range spec.Bootstrap {
		if len(argv) == 0 {
			continue
		}
		t.Logf("Bootstrapping external extension %s: %s", spec.Name, strings.Join(argv, " "))
		cmd := executil.Command(ctx, argv[0], argv[1:]...)
		cmd.Dir = cloneDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("bootstrap %q for %s failed: %w", strings.Join(argv, " "), spec.Name, err)
		}
	}

	configureArgs := append([]string{"--with-pgconfig=" + pgConfig}, spec.ConfigureArgs...)
	t.Logf("Configuring external extension %s: ./configure %s", spec.Name, strings.Join(configureArgs, " "))
	configure := executil.Command(ctx, "./configure", configureArgs...)
	configure.Dir = cloneDir
	configure.Stdout = os.Stdout
	configure.Stderr = os.Stderr
	if err := configure.Run(); err != nil {
		return fmt.Errorf("configure %s failed: %w", spec.Name, err)
	}

	t.Logf("Building external extension %s (make)...", spec.Name)
	makeCmd := executil.Command(ctx, "make", "-C", cloneDir, "-j", "4")
	makeCmd.Stdout = os.Stdout
	makeCmd.Stderr = os.Stderr
	if err := makeCmd.Run(); err != nil {
		return fmt.Errorf("make %s failed: %w", spec.Name, err)
	}

	t.Logf("Installing external extension %s into %s...", spec.Name, b.InstallDir)
	installCmd := executil.Command(ctx, "make", "-C", cloneDir, "install")
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("make %s install failed: %w", spec.Name, err)
	}
	return nil
}

// Cleanup removes per-invocation build and install artifacts but leaves the
// shared source checkout in place so subsequent runs skip the clone.
func (b *Builder) Cleanup() {
	if b.BuildDir != "" {
		buildRoot := filepath.Dir(b.BuildDir)
		_ = os.RemoveAll(buildRoot)
	}
}
