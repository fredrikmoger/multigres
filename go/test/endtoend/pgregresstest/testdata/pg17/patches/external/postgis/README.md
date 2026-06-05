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
