# PostGIS Support

Status: **spatial queries pass through multigateway cleanly and are covered by
automated tests.** This doc records how PostGIS is supported, how it's tested,
and what remains.

## TL;DR

PostGIS works through Multigres without any changes to the query path. The proxy
was built as a faithful PostgreSQL reimplementation rather than a
feature-allowlisted shim, so the three things that could have blocked PostGIS are
already handled:

1. **The parser accepts PostGIS syntax.** The lexer tokenizes arbitrary operator
   strings (`&&`, `<->`, `@>`, `|>>`, …) and the grammar already has productions
   for every DDL the extension's install script emits (`CREATE
EXTENSION/TYPE/OPERATOR/CAST/AGGREGATE`, `... USING gist`). `ST_*` calls are
   ordinary `FuncCall` nodes; type names are not allowlisted.
2. **Dynamic type OIDs pass through opaquely.** PostGIS assigns the `geometry`,
   `geography`, … type OIDs at `CREATE EXTENSION` time, so they are unknown at
   compile time. Every place the gateway switches on a type OID has a safe
   default (bind-param decoding falls back to opaque bytes; result rows forward
   the raw `data_type_oid`; an unknown type name in `PREPARE` resolves to OID 0,
   "infer at the backend").
3. **The safety filters don't touch PostGIS.** The gateway's Tier-2 statement
   blocklist (`LOAD`, `ALTER SYSTEM`, `CREATE SERVER`, …) and expression
   blocklist (`dblink`, `pg_read_file`, …) target none of what PostGIS uses; its
   DDL falls through to default routing, and literal normalization preserves
   `::geometry` casts and WKB literals. See
   [unsafe_statement_rejection.md](./unsafe_statement_rejection.md), which
   explicitly lists PostGIS as something `CREATE EXTENSION` must not break.

This was confirmed empirically before any harness work (see Phase 0 below).

## Two PostgreSQL environments, two test layers

PostGIS coverage is split across the two PostgreSQL builds the test suites use,
because each validates a different thing:

| Layer                        | PostgreSQL                        | PostGIS                        | What it proves                                                                                                                                           | Where                             |
| ---------------------------- | --------------------------------- | ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------- |
| **Integration suite**        | supabase/postgres image (PG 17.x) | prebuilt 3.3.x                 | spatial queries behave identically through the gateway and against direct PG                                                                             | `go/test/endtoend/postgis/`       |
| **pgregress external suite** | built from source (`REL_17_6`)    | built from source (pinned tag) | PostGIS builds + installs from source, `CREATE EXTENSION postgis` works through the gateway, and a curated spatial suite matches committed golden output | `go/test/endtoend/pgregresstest/` |

### Integration suite (`go/test/endtoend/postgis/`)

Runs against the supabase Postgres image, which already ships PostGIS, so it needs
no from-source build.

- **`postgis_smoke_test.go`** — boots a cluster, installs PostGIS through the
  gateway, and exercises the surface most likely to expose proxy bugs: the
  `CREATE EXTENSION` data load (`spatial_ref_sys` populated), a `::geometry`
  cast, `ST_DWithin`, the `&&` operator on a GiST index, an extended-protocol
  parameterized query, and — the case flagged as the real risk — an
  **extended-protocol Bind carrying a binary-format parameter typed with the
  dynamic `geometry` OID**.
- **`comparison_test.go`** — the triage harness. It runs a broad spatial surface
  (text I/O, measurement, geography, predicates, operators, GiST `&&`, KNN
  `<->`, parameterized queries, `EXPLAIN`, binary result format, and error
  paths) against **both** a direct PostgreSQL connection and the gateway, and
  asserts they agree — same rows, same SQLSTATE on errors. Because both
  connections target the same primary, the direct connection is the oracle and
  any divergence is purely gateway-induced (version and precision never enter
  in). This is where a proxy bug in the spatial path would surface.

### pgregress external suite

The PostgreSQL regression harness builds Postgres from source and now builds
PostGIS from source alongside it, then runs a curated spatial suite through the
gateway.

- **Build hook** ([pgbuilder/builder.go](../../go/test/endtoend/pgbuilder/builder.go)):
  `InstallExternalExtension` takes an `ExtSpec` and supports two build systems.
  pgvector is `ExtBuildPGXS` (`make PG_CONFIG=…`; `make install`). PostGIS is
  `ExtBuildAutotools` — `./autogen.sh`, then `./configure --with-pgconfig=…`,
  `make`, `make install` — because its top-level configure probes for
  GEOS/PROJ/libxml2 and drives the PGXS build underneath.
- **Registration** ([extensions.go](../../go/test/endtoend/pgregresstest/extensions.go)):
  `postgis` is `StatusCovered` with an `externalSpecs` entry pinning the repo,
  tag, and configure flags (`--without-raster --without-protobuf` to keep the
  native-library footprint to GEOS/PROJ/libxml2/json-c).
- **Curated suite**
  ([testdata/pg17/external/postgis/](../../go/test/endtoend/pgregresstest/testdata/pg17/external/postgis/)):
  PostGIS doesn't ship pgvector's PGXS `test/sql` layout (its upstream suite is
  Perl-driven), so the external runner uses an in-repo curated suite when one
  exists. The suite is deliberately version-stable (integer geometry I/O,
  booleans, exact distances) so its golden output stays valid as the pinned tag
  advances. Intended, benign diffs go under
  [patches/external/postgis/](../../go/test/endtoend/pgregresstest/testdata/pg17/patches/external/postgis/);
  a divergence that is a real proxy bug should be fixed in the gateway instead.
- **CI**: [test-pgregress.yml](../../.github/workflows/test-pgregress.yml)
  installs the PostGIS build deps (`autoconf pkg-config libgeos-dev libproj-dev
libxml2-dev libjson-c-dev`).

## Phase status

| Phase | What                                                               | Status                              |
| ----- | ------------------------------------------------------------------ | ----------------------------------- |
| 0     | Empirical smoke test — prove spatial queries survive the gateway   | ✅ done & passing                   |
| 1     | Build PostGIS into the managed Postgres (autotools build hook)     | ✅ implemented                      |
| 2     | Register in the harness (catalog + spec + curated suite + CI deps) | ✅ implemented                      |
| 3     | Triage proxy gaps (comparison harness; patches for intended diffs) | ✅ harness in place, baseline clean |

## Running it locally

```bash
# Integration suite (needs a PostGIS-enabled Postgres on PATH, e.g. the
# supabase/postgres image; see docs/supabase-postgres-testing.md):
go test -v -run TestPostGIS ./go/test/endtoend/postgis/...

# pgregress external suite (builds Postgres + PostGIS from source; needs the
# build deps from test-pgregress.yml):
RUN_PGEXTERNAL=1 PGEXTERNAL_TESTS=postgis \
  go test -v -run TestPostgreSQLRegression ./go/test/endtoend/pgregresstest/...
```

## Not yet covered / follow-ups

- **PostGIS's full upstream regression suite.** It is Perl-driven
  (`regress/run_test.pl`) with `@`-substitutions and loader fixtures, so it
  isn't drop-in for the PGXS-style external runner. The curated suite covers the
  core surface through the gateway; adapting the full upstream suite is a larger
  follow-on and the highest-value way to widen Phase-3 coverage.
- **Raster (`postgis_raster`) and `postgis_topology`** suites. Topology is
  installed by the same build; raster is disabled (`--without-raster`) to avoid
  the GDAL dependency. Both are out of scope for now.
- **Sharding.** This work makes PostGIS work behind a single-shard Multigres
  (pooling + HA for spatial workloads). Routing spatial queries across shards is
  a separate, much larger problem that does not exist in the codebase yet.

## Note for Atlas

Atlas's GIS repositories are heavily PostGIS-dependent, so PostGIS-through-the-proxy
is a hard prerequisite for any future adoption of Multigres at Atlas. The result
here is encouraging: the spatial path passes through cleanly today, and the
near-term value (a transparent connection pooler + HA in front of a
PostGIS-enabled Postgres) is available without waiting on sharding. The blocking
caveat from the earlier assessment — "PostGIS is untested through the gateway" —
is now addressed by the test layers above; the remaining gating item for
production reliance is breadth (the full upstream suite) and the project's
overall alpha maturity.
