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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// samplePostGISOutput is real run_test.pl output the parser must handle.
const samplePostGISOutput = `Running tests

 regress/core/affine .. ok in 17 ms
 regress/core/measures .. failed (diff expected obtained: /tmp/pgis_reg/test_2_diff)
 regress/core/operators .. failed (diff expected obtained: /tmp/pgis_reg/test_3_diff)
 regress/core/binary .. ok in 22 ms
 regress/core/wkb .. ok in 21 ms
 regress/loader/Point .. ok in 30 ms

Run tests: 6
Failed: 2
`

func TestParsePostGISResults(t *testing.T) {
	got := parsePostGISResults(samplePostGISOutput)
	assert.Equal(t, map[string]bool{
		"core/affine":    true,
		"core/measures":  false,
		"core/operators": false,
		"core/binary":    true,
		"core/wkb":       true,
		"loader/Point":   true,
	}, got)
}

func TestParsePostGISResults_noResultLines(t *testing.T) {
	assert.Empty(t, parsePostGISResults("Using existing database postgres\nRun tests: 0\n"))
}

func TestClassifyPostGISResults(t *testing.T) {
	tests := []string{
		"core/pass_both",
		"core/fail_direct",
		"core/gateway_only",
		"core/patched",
		"core/missing_gateway",
	}
	direct := map[string]bool{
		"core/pass_both":       true,
		"core/fail_direct":     false,
		"core/gateway_only":    true,
		"core/patched":         true,
		"core/missing_gateway": true,
	}
	gateway := map[string]bool{
		"core/pass_both":    true,
		"core/fail_direct":  false,
		"core/gateway_only": false,
		"core/patched":      false,
		// core/missing_gateway intentionally absent: didn't run on the gateway
	}
	hasPatch := func(test string) bool { return test == "core/patched" }
	patchPath := func(test string) string { return "patches/" + test + ".patch" }

	res := classifyPostGISResults(tests, direct, gateway, hasPatch, patchPath)

	byName := map[string]IndividualTestResult{}
	for _, tr := range res.Tests {
		byName[tr.Name] = tr
	}

	assert.Equal(t, "pass", byName["postgis/core/pass_both"].Status)
	assert.Equal(t, "skip", byName["postgis/core/fail_direct"].Status,
		"fails on direct PG too -> not a gateway regression")
	assert.Equal(t, "fail", byName["postgis/core/gateway_only"].Status,
		"passes direct, fails gateway -> the only real failure")
	assert.Equal(t, "pass", byName["postgis/core/patched"].Status)
	assert.True(t, byName["postgis/core/patched"].PatchApplied)
	assert.Equal(t, "patches/core/patched.patch", byName["postgis/core/patched"].PatchPath)
	assert.Equal(t, "skip", byName["postgis/core/missing_gateway"].Status)

	assert.Equal(t, 2, res.PassedTests)  // pass_both + patched
	assert.Equal(t, 1, res.FailedTests)  // gateway_only
	assert.Equal(t, 2, res.SkippedTests) // fail_direct + missing_gateway
	assert.Equal(t, 5, res.TotalTests)
	assert.Len(t, res.FailureDetails, 1)
	assert.Equal(t, "postgis/core/gateway_only", res.FailureDetails[0].TestName)
}

func TestPostGISCoreTests(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "core"), 0o755))
	for _, f := range []string{"affine.sql", "measures.sql", "binary.sql"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "core", f), nil, 0o644))
	}

	got, err := postgisCoreTests(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"core/affine", "core/binary", "core/measures"}, got)

	// POSTGIS_CORE_TESTS restricts to the named leaves (with or without prefix).
	t.Setenv(postgisCoreTestsEnv, "affine core/measures")
	got, err = postgisCoreTests(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"core/affine", "core/measures"}, got)
}

func TestPostGISExtraTests(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "loader"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "dumper"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "tests.mk"), []byte(
		"TESTS += \\\n\t$(top_srcdir)/regress/loader/Point \\\n\t$(top_srcdir)/regress/loader/Polygon\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dumper", "tests.mk"), []byte(
		"TESTS += \\\n\t$(top_srcdir)/regress/dumper/literalsrid\n"), 0o644))

	got := postgisExtraTests(dir)
	assert.Equal(t, []string{"dumper/literalsrid", "loader/Point", "loader/Polygon"}, got)
}
