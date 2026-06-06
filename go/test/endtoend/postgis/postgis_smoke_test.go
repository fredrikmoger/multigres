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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/multigres/multigres/go/common/pgprotocol/client"
	"github.com/multigres/multigres/go/common/sqltypes"
	"github.com/multigres/multigres/go/test/endtoend/shardsetup"
	"github.com/multigres/multigres/go/test/utils"
)

// TestPostGISSmoke installs PostGIS through multigateway and checks that the
// spatial surface most likely to expose proxy bugs survives the gateway: a
// geometry cast, ST_DWithin, the && operator on a GiST index, an extended-protocol
// parameterized query, and a binary-format geometry bind param (dynamic OID).
// Read queries run against both direct PostgreSQL and the gateway.
func TestPostGISSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PostGIS smoke test in short mode")
	}
	if utils.ShouldSkipRealPostgres() {
		t.Skip("PostgreSQL binaries not found, skipping PostGIS smoke test")
	}

	setup := getSharedSetup(t)
	setup.SetupTest(t)
	ctx := utils.WithTimeout(t, 120*time.Second)

	gwDSN := shardsetup.GetTestUserDSN("localhost", setup.MultigatewayPgPort, "sslmode=disable", "connect_timeout=5")

	gw, err := pgx.Connect(ctx, gwDSN)
	require.NoError(t, err, "connect to multigateway")
	defer gw.Close(ctx)

	if _, err := gw.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS postgis"); err != nil {
		t.Skipf("PostGIS not available in this PostgreSQL build (%v); skipping", err)
	}

	t.Cleanup(func() {
		// fresh context: the test ctx may be expired by cleanup time.
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanup, err := pgx.Connect(cctx, gwDSN)
		if err != nil {
			return
		}
		defer cleanup.Close(cctx)
		_, _ = cleanup.Exec(cctx, "DROP TABLE IF EXISTS postgis_smoke")
		_, _ = cleanup.Exec(cctx, "DROP EXTENSION IF EXISTS postgis CASCADE")
	})

	var postgisVersion string
	require.NoError(t, gw.QueryRow(ctx, "SELECT postgis_version()").Scan(&postgisVersion))
	t.Logf("PostGIS version (via multigateway): %s", postgisVersion)

	var srsCount int
	require.NoError(t, gw.QueryRow(ctx, "SELECT count(*) FROM spatial_ref_sys").Scan(&srsCount))
	assert.Greater(t, srsCount, 1000, "spatial_ref_sys should be populated by CREATE EXTENSION")
	t.Logf("spatial_ref_sys rows (via multigateway): %d", srsCount)

	_, err = gw.Exec(ctx, `CREATE TABLE postgis_smoke (
		id   int PRIMARY KEY,
		name text,
		geom geometry(Point, 4326)
	)`)
	require.NoError(t, err, "CREATE TABLE with geometry column")

	// Two points near Oslo, one in Bergen (far west).
	_, err = gw.Exec(ctx, `INSERT INTO postgis_smoke (id, name, geom) VALUES
		(1, 'oslo-a',  ST_SetSRID(ST_MakePoint(10.75, 59.91), 4326)),
		(2, 'oslo-b',  ST_SetSRID(ST_MakePoint(10.76, 59.92), 4326)),
		(3, 'bergen',  ST_SetSRID(ST_MakePoint(5.32, 60.39), 4326))`)
	require.NoError(t, err, "INSERT geometries built with ST_MakePoint/ST_SetSRID")

	_, err = gw.Exec(ctx, `CREATE INDEX postgis_smoke_gix ON postgis_smoke USING gist (geom)`)
	require.NoError(t, err, "CREATE INDEX ... USING gist (geom)")

	for _, target := range setup.GetComparisonTargets(t) {
		t.Run(target.Name, func(t *testing.T) {
			dsn := shardsetup.GetTestUserDSN("localhost", target.Port, "sslmode=disable", "connect_timeout=5")
			conn, err := pgx.Connect(ctx, dsn)
			require.NoError(t, err)
			defer conn.Close(ctx)

			t.Run("geometry cast + ST_AsText", func(t *testing.T) {
				var wkt string
				require.NoError(t, conn.QueryRow(ctx,
					"SELECT ST_AsText('POINT(10.75 59.91)'::geometry)").Scan(&wkt))
				assert.Equal(t, "POINT(10.75 59.91)", wkt)
			})

			t.Run("ST_DWithin filter", func(t *testing.T) {
				var n int
				require.NoError(t, conn.QueryRow(ctx, `
					SELECT count(*) FROM postgis_smoke
					WHERE ST_DWithin(geom, ST_SetSRID(ST_MakePoint(10.75, 59.91), 4326), 0.05)`).Scan(&n))
				assert.Equal(t, 2, n, "the two Oslo points are within 0.05deg; Bergen is not")
			})

			t.Run("&& bounding-box operator (GiST)", func(t *testing.T) {
				var n int
				require.NoError(t, conn.QueryRow(ctx, `
					SELECT count(*) FROM postgis_smoke
					WHERE geom && ST_MakeEnvelope(10.0, 59.0, 11.0, 60.0, 4326)`).Scan(&n))
				assert.Equal(t, 2, n, "envelope around Oslo contains the two Oslo points")
			})

			t.Run("parameterized spatial query (extended protocol)", func(t *testing.T) {
				// pgx uses the extended protocol; the floats flow as bind params.
				var dist float64
				require.NoError(t, conn.QueryRow(ctx, `
					SELECT ST_Distance(
						ST_SetSRID(ST_MakePoint($1::float8, $2::float8), 4326),
						ST_SetSRID(ST_MakePoint($3::float8, $4::float8), 4326))`,
					10.75, 59.91, 10.76, 59.92).Scan(&dist))
				assert.InDelta(t, 0.01414, dist, 0.0001, "distance between the two Oslo points (degrees)")
			})
		})
	}

	// The risk case: a binary-format bind param typed with the runtime geometry
	// OID — the gateway has no compile-time knowledge of it and must forward the
	// bytes verbatim.
	t.Run("binary geometry bind param via raw protocol", func(t *testing.T) {
		// Fetch the runtime geometry OID and a canonical WKB from the server.
		var geomOID uint32
		require.NoError(t, gw.QueryRow(ctx, "SELECT 'geometry'::regtype::oid").Scan(&geomOID))
		require.NotZero(t, geomOID)

		var wkb []byte
		require.NoError(t, gw.QueryRow(ctx, "SELECT ST_AsBinary(ST_Point(1, 2))").Scan(&wkb))
		require.NotEmpty(t, wkb)

		conn, err := client.Connect(ctx, ctx, &client.Config{
			Host:        "localhost",
			Port:        setup.MultigatewayPgPort,
			User:        shardsetup.DefaultTestUser,
			Password:    shardsetup.TestPostgresPassword,
			Database:    "postgres",
			DialTimeout: 5 * time.Second,
		})
		require.NoError(t, err)
		defer conn.Close()

		require.NoError(t, conn.Parse(ctx, "geom_bin", "SELECT ST_AsText($1)", []uint32{geomOID}))

		var results []*sqltypes.Result
		_, err = conn.BindAndExecute(ctx, "", "geom_bin",
			[][]byte{wkb}, // param value: server-produced WKB
			[]int16{1},    // param format: binary
			[]int16{0},    // result format: text
			0,             // unlimited rows
			func(_ context.Context, r *sqltypes.Result) error {
				results = append(results, r)
				return nil
			})
		require.NoError(t, err, "binary geometry bind param must pass through the gateway")

		var got string
		for _, r := range results {
			if len(r.Rows) > 0 {
				got = string(r.Rows[0].Values[0])
				break
			}
		}
		assert.Equal(t, "POINT(1 2)", got)

		require.NoError(t, conn.CloseStatement(ctx, "geom_bin"))
	})
}
