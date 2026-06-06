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

package postgis

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/multigres/multigres/go/test/endtoend/shardsetup"
	"github.com/multigres/multigres/go/test/utils"
)

// TestPostGISGatewayMatchesDirect runs a broad PostGIS surface against direct
// PostgreSQL and the gateway and asserts they agree. Both target the same
// primary, so direct PostgreSQL is the oracle and any divergence is the gateway's.
func TestPostGISGatewayMatchesDirect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PostGIS comparison test in short mode")
	}
	if utils.ShouldSkipRealPostgres() {
		t.Skip("PostgreSQL binaries not found, skipping PostGIS comparison test")
	}

	setup := getSharedSetup(t)
	setup.SetupTest(t)
	ctx := utils.WithTimeout(t, 120*time.Second)

	targets := setup.GetComparisonTargets(t)
	var directPort, gatewayPort int
	for _, tgt := range targets {
		switch tgt.Name {
		case "postgres":
			directPort = tgt.Port
		case "multigateway":
			gatewayPort = tgt.Port
		}
	}
	require.NotZero(t, directPort, "direct postgres target")
	require.NotZero(t, gatewayPort, "multigateway target")

	open := func(port int) *sql.DB {
		db, err := sql.Open("postgres", shardsetup.GetTestUserDSN("localhost", port, "sslmode=disable", "connect_timeout=5"))
		require.NoError(t, err)
		db.SetMaxOpenConns(2)
		return db
	}
	direct := open(directPort)
	defer direct.Close()
	gateway := open(gatewayPort)
	defer gateway.Close()

	// Both targets are the same primary, so seed once (and clean up after).
	// Skip (don't fail) when this PostgreSQL build lacks PostGIS, e.g. the generic
	// integration suite — only the supabase image and the from-source build have it.
	if _, err := gateway.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS postgis"); err != nil {
		t.Skipf("PostGIS not available in this PostgreSQL build: %v", err)
	}
	if !postgisInstalled(ctx, gateway) {
		t.Skip("PostGIS not available in this PostgreSQL build")
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = gateway.ExecContext(cctx, "DROP TABLE IF EXISTS cmp_postgis")
		_, _ = gateway.ExecContext(cctx, "DROP EXTENSION IF EXISTS postgis CASCADE")
	})

	mustExec(t, ctx, gateway, `CREATE TABLE cmp_postgis (
		id int PRIMARY KEY,
		name text,
		geom geometry(Point, 4326)
	)`)
	mustExec(t, ctx, gateway, `INSERT INTO cmp_postgis (id, name, geom) VALUES
		(1, 'origin',  ST_SetSRID(ST_MakePoint(0, 0), 4326)),
		(2, 'near',    ST_SetSRID(ST_MakePoint(1, 1), 4326)),
		(3, 'mid',     ST_SetSRID(ST_MakePoint(5, 5), 4326)),
		(4, 'far',     ST_SetSRID(ST_MakePoint(50, 50), 4326))`)
	mustExec(t, ctx, gateway, "CREATE INDEX cmp_postgis_gix ON cmp_postgis USING gist (geom)")
	mustExec(t, ctx, gateway, "ANALYZE cmp_postgis")

	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{"astext_cast", "SELECT ST_AsText('POINT(1 2)'::geometry)", nil},
		{"asewkt_srid", "SELECT ST_AsEWKT(ST_SetSRID(ST_MakePoint(1, 2), 4326))", nil},
		{"asgeojson", "SELECT ST_AsGeoJSON(ST_MakePoint(1, 2))", nil},
		{"ashexewkb", "SELECT ST_AsHEXEWKB(ST_SetSRID(ST_MakePoint(1, 2), 4326))", nil},
		{"geometrytype", "SELECT GeometryType('LINESTRING(0 0,1 1)'::geometry)", nil},
		{"st_geometrytype", "SELECT ST_GeometryType('POLYGON((0 0,0 1,1 1,1 0,0 0))'::geometry)", nil},
		{"npoints", "SELECT ST_NPoints('LINESTRING(0 0,1 1,2 2)'::geometry)", nil},
		{"area", "SELECT ST_Area('POLYGON((0 0,0 2,2 2,2 0,0 0))'::geometry)", nil},
		{"length", "SELECT ST_Length('LINESTRING(0 0,3 4)'::geometry)", nil},
		{"distance", "SELECT ST_Distance(ST_MakePoint(0, 0), ST_MakePoint(3, 4))", nil},
		{"centroid_x", "SELECT ST_X(ST_Centroid('LINESTRING(0 0,2 0)'::geometry))", nil},
		{"geog_distance", "SELECT ST_Distance('POINT(0 0)'::geography, 'POINT(0 1)'::geography)", nil},
		{"geog_dwithin", "SELECT ST_DWithin('POINT(0 0)'::geography, 'POINT(0 1)'::geography, 200000)", nil},
		{"contains", "SELECT ST_Contains('POLYGON((0 0,0 10,10 10,10 0,0 0))'::geometry, 'POINT(5 5)'::geometry)", nil},
		{"intersects", "SELECT ST_Intersects('LINESTRING(0 0,10 10)'::geometry, 'LINESTRING(0 10,10 0)'::geometry)", nil},
		{"bbox_overlap", "SELECT 'POINT(0 0)'::geometry && 'POLYGON((-1 -1,-1 1,1 1,1 -1,-1 -1))'::geometry", nil},
		{"bbox_count", "SELECT count(*) FROM cmp_postgis WHERE geom && ST_MakeEnvelope(-1, -1, 2, 2, 4326)", nil},
		{"knn_order", "SELECT id FROM cmp_postgis ORDER BY geom <-> ST_SetSRID(ST_MakePoint(0, 0), 4326) LIMIT 3", nil},
		{"dwithin_rows", "SELECT id, name FROM cmp_postgis WHERE ST_DWithin(geom, ST_SetSRID(ST_MakePoint(0, 0), 4326), 2) ORDER BY id", nil},
		{"param_makepoint", "SELECT ST_AsText(ST_SetSRID(ST_MakePoint($1::float8, $2::float8), 4326))", []any{10.5, 20.25}},
		{"param_dwithin", "SELECT count(*) FROM cmp_postgis WHERE ST_DWithin(geom, ST_SetSRID(ST_MakePoint($1::float8, $2::float8), 4326), $3::float8)", []any{0.0, 0.0, 8.0}},
		{"err_bad_wkt_cast", "SELECT 'NOT WKT'::geometry", nil},
		{"err_bad_geomfromtext", "SELECT ST_GeomFromText('GIBBERISH')", nil},
		{"err_srid_mismatch", "SELECT ST_Intersects(ST_SetSRID(ST_MakePoint(0,0),4326), ST_SetSRID(ST_MakePoint(0,0),3857))", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dRows, dErr := queryText(ctx, direct, tc.query, tc.args...)
			gRows, gErr := queryText(ctx, gateway, tc.query, tc.args...)

			if dErr != nil || gErr != nil {
				require.Error(t, dErr, "direct PG unexpectedly succeeded for %q", tc.query)
				require.Error(t, gErr, "gateway unexpectedly succeeded for %q (direct errored)", tc.query)
				assert.Equal(t, sqlState(dErr), sqlState(gErr),
					"SQLSTATE mismatch for %q\n direct: %v\ngateway: %v", tc.query, dErr, gErr)
				return
			}
			assert.Equal(t, dRows, gRows, "row mismatch through gateway for %q", tc.query)
		})
	}

	t.Run("explain_gist", func(t *testing.T) {
		const q = "EXPLAIN (COSTS OFF) SELECT id FROM cmp_postgis WHERE geom && ST_MakeEnvelope(-1, -1, 2, 2, 4326)"
		dRows, dErr := queryText(ctx, direct, q)
		gRows, gErr := queryText(ctx, gateway, q)
		require.NoError(t, dErr)
		require.NoError(t, gErr)
		assert.Equal(t, dRows, gRows, "EXPLAIN plan differs through the gateway")
	})

	// Geometry returned in binary (EWKB): the gateway must forward the result
	// bytes for the dynamic geometry OID untouched.
	t.Run("binary_result_format", func(t *testing.T) {
		fetch := func(port int) []byte {
			dsn := shardsetup.GetTestUserDSN("localhost", port, "sslmode=disable", "connect_timeout=5")
			conn, err := pgx.Connect(ctx, dsn)
			require.NoError(t, err)
			defer conn.Close(ctx)
			rows, err := conn.Query(ctx, "SELECT ST_SetSRID(ST_MakePoint(1, 2), 4326)",
				pgx.QueryResultFormats{pgx.BinaryFormatCode})
			require.NoError(t, err)
			defer rows.Close()
			require.True(t, rows.Next())
			vals := rows.RawValues()
			require.Len(t, vals, 1)
			out := append([]byte(nil), vals[0]...)
			require.NoError(t, rows.Err())
			return out
		}
		assert.Equal(t, fetch(directPort), fetch(gatewayPort),
			"binary EWKB result bytes differ through the gateway")
	})
}

// queryText renders rows as tab-joined text (NULL as "NULL") so results from two
// connections compare regardless of column type.
func queryText(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		parts := make([]string, len(cols))
		for i, c := range cells {
			switch v := c.(type) {
			case nil:
				parts[i] = "NULL"
			case []byte:
				parts[i] = string(v)
			default:
				parts[i] = fmt.Sprint(v)
			}
		}
		out = append(out, strings.Join(parts, "\t"))
	}
	return out, rows.Err()
}

// sqlState extracts the PostgreSQL SQLSTATE from a lib/pq error, or "" if absent.
func sqlState(err error) string {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return string(pqErr.Code)
	}
	return ""
}

func mustExec(t *testing.T, ctx context.Context, db *sql.DB, stmt string) {
	t.Helper()
	_, err := db.ExecContext(ctx, stmt)
	require.NoError(t, err, "exec: %s", stmt)
}

func postgisInstalled(ctx context.Context, db *sql.DB) bool {
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pg_extension WHERE extname = 'postgis'").Scan(&n); err != nil {
		return false
	}
	return n > 0
}
