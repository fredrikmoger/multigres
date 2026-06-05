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

package pgregresstest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/multigres/multigres/go/test/endtoend/suiteutil"
	"github.com/multigres/multigres/go/tools/executil"
)

// PostGIS's upstream suite is driven by a Perl harness (regress/run_test.pl), not
// pg_regress. With --nocreate --nodrop it runs against a pre-existing database
// over libpq env vars, which fits the gateway (one fixed "postgres" database, no
// CREATE DATABASE). This runner drives it against both direct PostgreSQL and the
// gateway and reports only tests that pass directly but fail through the gateway,
// so upstream/version noise doesn't masquerade as a proxy bug. Deep companion to
// the fast curated smoke suite in testdata/pg17/external/postgis.

const (
	runPostGISCoreEnv   = "RUN_POSTGIS_CORE"   // gate the deep core suite (off by default; ~141 tests run twice)
	runPostGISLoaderEnv = "RUN_POSTGIS_LOADER" // also run loader/dumper tests (need built shp2pgsql/pgsql2shp)
	postgisCoreTestsEnv = "POSTGIS_CORE_TESTS" // subset of core tests by leaf name, space-separated
)

// PostGISCoreSuiteEnabled reports whether the deep PostGIS core suite should run.
func PostGISCoreSuiteEnabled() bool {
	return os.Getenv(runPostGISCoreEnv) != ""
}

func postgisLoaderSuiteEnabled() bool {
	return os.Getenv(runPostGISLoaderEnv) != ""
}

// postgisResultLine matches run_test.pl verdict lines, e.g. " regress/core/affine .. ok in 17 ms".
var postgisResultLine = regexp.MustCompile(`(?m)^\s*(\S+?)\s+\.+\s+(ok|failed)\b`)

// parsePostGISResults turns run_test.pl output into a test -> passed map, keyed by
// normalized test path (the leading "regress/" stripped, e.g. "core/affine").
func parsePostGISResults(out string) map[string]bool {
	res := map[string]bool{}
	for _, m := range postgisResultLine.FindAllStringSubmatch(out, -1) {
		name := strings.TrimPrefix(m[1], "regress/")
		res[name] = m[2] == "ok"
	}
	return res
}

// classifyPostGISResults reports only gateway-induced failures: pass-on-both is a
// pass; fail-on-direct is a skip (upstream/version noise); pass-direct-fail-gateway
// is a fail unless a reviewed-benign marker exists (hasPatch). hasPatch/patchPath
// are injected to keep this pure and unit-testable.
func classifyPostGISResults(tests []string, direct, gateway map[string]bool, hasPatch func(test string) bool, patchPath func(test string) string) *TestResults {
	results := &TestResults{
		FailureDetails: []TestFailure{},
		Tests:          []IndividualTestResult{},
	}
	for _, test := range tests {
		name := "postgis/" + test
		dPass, dHas := direct[test]
		gPass, gHas := gateway[test]

		switch {
		case !dHas || !gHas:
			results.Tests = append(results.Tests, IndividualTestResult{
				Name: name, Status: "skip", FailReason: "did not run on both targets",
			})
			results.SkippedTests++
		case !dPass:
			results.Tests = append(results.Tests, IndividualTestResult{
				Name: name, Status: "skip",
				FailReason: "fails on direct PostgreSQL (upstream/version, not gateway)",
			})
			results.SkippedTests++
		case dPass && gPass:
			results.Tests = append(results.Tests, IndividualTestResult{Name: name, Status: "pass"})
			results.PassedTests++
		default: // dPass && !gPass -> gateway-induced failure
			if hasPatch(test) {
				results.Tests = append(results.Tests, IndividualTestResult{
					Name: name, Status: "pass", PatchApplied: true, PatchPath: patchPath(test),
				})
				results.PassedTests++
			} else {
				results.Tests = append(results.Tests, IndividualTestResult{
					Name: name, Status: "fail",
					FailReason: "passes on direct PostgreSQL but fails through multigateway",
				})
				results.FailedTests++
				results.FailureDetails = append(results.FailureDetails, TestFailure{
					TestName: name,
					Error:    "gateway-induced failure (run the suite locally to see the run_test.pl diff)",
				})
			}
		}
	}
	results.TotalTests = results.PassedTests + results.FailedTests + results.SkippedTests
	return results
}

// postgisCoreTests enumerates regress/core/*.sql as sorted "core/<name>".
// POSTGIS_CORE_TESTS, if set, restricts the list to the named leaves.
func postgisCoreTests(regressDir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(regressDir, "core", "*.sql"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)

	var want map[string]bool
	if sel := strings.Fields(os.Getenv(postgisCoreTestsEnv)); len(sel) > 0 {
		want = make(map[string]bool, len(sel))
		for _, s := range sel {
			want[strings.TrimPrefix(s, "core/")] = true
		}
	}

	var out []string
	for _, m := range matches {
		leaf := strings.TrimSuffix(filepath.Base(m), ".sql")
		if want != nil && !want[leaf] {
			continue
		}
		out = append(out, "core/"+leaf)
	}
	return out, nil
}

// postgisExtraTests enumerates the loader/dumper tests from regress/{loader,dumper}/tests.mk.
func postgisExtraTests(regressDir string) []string {
	ref := regexp.MustCompile(`regress/((?:loader|dumper)/\S+)`)
	var out []string
	for _, sub := range []string{"loader", "dumper"} {
		data, err := os.ReadFile(filepath.Join(regressDir, sub, "tests.mk"))
		if err != nil {
			continue
		}
		for _, m := range ref.FindAllStringSubmatch(string(data), -1) {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func postgisPatchBase(test string) string {
	return filepath.Base(test)
}

// postgisPatch{Abs,Rel}Path locate a test's reviewed gateway-diff marker.
func postgisPatchAbsPath(test string) string {
	return filepath.Join(PatchesDir(), "external", "postgis", postgisPatchBase(test)+".patch")
}

func postgisPatchRelPath(test string) string {
	return filepath.Join("testdata", pgMajorDir(), "patches", "external", "postgis", postgisPatchBase(test)+".patch")
}

// runPostGISHarness runs run_test.pl for the given tests against host:port and
// returns its combined output (it exits non-zero on any failure; the caller parses).
func (pb *PostgresBuilder) runPostGISHarness(ctx context.Context, regressDir, cloneDir, host string, port int, user, password string, tests []string) (string, error) {
	args := append([]string{"run_test.pl", "--nocreate", "--nodrop", "--extensions"}, tests...)
	cmd := executil.Command(ctx, "perl", args...)
	cmd.SetDir(regressDir)
	cmd.SetEnv(append(os.Environ(),
		"PGHOST="+host,
		fmt.Sprintf("PGPORT=%d", port),
		"PGUSER="+user,
		"PGPASSWORD="+password,
		"PGDATABASE=postgres",
		"POSTGIS_REGRESS_DB=postgres",
		"POSTGIS_TOP_BUILD_DIR="+cloneDir, // checkout root: run_test.pl looks here for loader binaries
	))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// RunPostGISCoreSuite runs PostGIS's regression suite (core, plus loader/dumper
// when enabled) against direct PostgreSQL and the gateway, flagging only
// gateway-induced regressions. cloneDir is the checkout InstallExternalExtension
// produced; PostGIS must already be installed in the from-source PostgreSQL.
func (pb *PostgresBuilder) RunPostGISCoreSuite(t *testing.T, ctx context.Context, cloneDir string, multigatewayPort, directPgPort int, password string) (*TestResults, error) {
	t.Helper()

	regressDir := filepath.Join(cloneDir, "regress")
	runner := filepath.Join(regressDir, "run_test.pl")
	if !suiteutil.FileExists(runner) {
		return nil, fmt.Errorf("postgis checkout missing %s (was InstallExternalExtension run?)", runner)
	}

	tests, err := postgisCoreTests(regressDir)
	if err != nil {
		return nil, fmt.Errorf("enumerate postgis core tests: %w", err)
	}
	if postgisLoaderSuiteEnabled() {
		tests = append(tests, postgisExtraTests(regressDir)...)
	} else {
		t.Logf("postgis: loader/dumper tests skipped (set %s=1 to enable)", runPostGISLoaderEnv)
	}
	if len(tests) == 0 {
		return nil, fmt.Errorf("no postgis tests found under %s", regressDir)
	}

	// run_test.pl --nocreate needs PostGIS present; one create via the gateway serves both runs (same primary).
	if err := createExtensionViaGateway(multigatewayPort, password, "postgis"); err != nil {
		return nil, fmt.Errorf("pre-create postgis via gateway: %w", err)
	}

	t.Logf("postgis: running %d tests against direct PostgreSQL (port %d)...", len(tests), directPgPort)
	directOut, _ := pb.runPostGISHarness(ctx, regressDir, cloneDir, "localhost", directPgPort, "postgres", password, tests)
	direct := parsePostGISResults(directOut)

	t.Logf("postgis: running %d tests through multigateway (port %d)...", len(tests), multigatewayPort)
	gatewayOut, _ := pb.runPostGISHarness(ctx, regressDir, cloneDir, "localhost", multigatewayPort, "postgres", password, tests)
	gateway := parsePostGISResults(gatewayOut)

	if len(direct) == 0 && len(gateway) == 0 {
		return nil, fmt.Errorf("run_test.pl produced no parseable results\n--- direct ---\n%s\n--- gateway ---\n%s",
			truncateForLog(directOut, 2000), truncateForLog(gatewayOut, 2000))
	}

	results := classifyPostGISResults(tests, direct, gateway,
		func(test string) bool { return suiteutil.FileExists(postgisPatchAbsPath(test)) },
		postgisPatchRelPath,
	)
	t.Logf("postgis: %d passed, %d gateway-induced failures, %d skipped (direct-fail/not-run)",
		results.PassedTests, results.FailedTests, results.SkippedTests)
	return results, nil
}
