# PostGIS external-suite patches

Per-test patches for intended, benign differences between PostGIS output through
multigateway and the committed `expected/` golden (see
`../../external/postgis/`). The external runner applies `<test>.patch` here
before deciding pass/fail, exactly like the contrib modules.

Empty for now: the curated suite (`external/postgis/sql/postgis.sql`) is chosen
to be byte-identical through the gateway and across PostGIS minor versions, so
no intended diffs exist yet. Add a `<test>.patch` here only when a divergence is
reviewed and judged benign (e.g. a NOTICE-ordering or error-text difference that
faithfully reflects gateway behavior). A divergence that is a real proxy bug
should be fixed in the gateway instead.

This directory is shared by two runners:

- the **curated suite** (`RunExternalTests`), where a `<test>.patch` is a unified
  diff applied to the committed `expected/` before diffing, and
- the **deep suite** (`postgis_suite.go`, driven by PostGIS's `run_test.pl`),
  where the runner already compares the gateway run against a direct-PostgreSQL
  run; here a `<base>.patch` acts as a **marker** that a gateway-only failure for
  that test (`core/<base>`, `loader/<base>`, …) has been reviewed and accepted,
  downgrading it from a failure to a patched pass. Put the captured `run_test.pl`
  diff in the file so the reason is reviewable.
