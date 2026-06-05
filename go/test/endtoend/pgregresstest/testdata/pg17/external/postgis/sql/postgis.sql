-- Curated PostGIS spatial regression suite, run through multigateway.
--
-- PostGIS does not ship the PGXS test/sql + expected layout that the external
-- suite expects (its upstream suite is Perl-driven), so this small, in-repo
-- suite exercises the spatial surface most likely to expose proxy bugs. Every
-- query is chosen to produce output that is identical across PostGIS minor
-- versions (integer geometry I/O, boolean predicates, exact distances, SRID
-- integers) so the committed expected output stays valid as the pinned build
-- tag advances. The extension is preloaded by the harness (--load-extension),
-- so this file must not CREATE EXTENSION.

-- Geometry text I/O: the ::geometry cast and ST_AsText round-trip.
SELECT ST_AsText('POINT(1 2)'::geometry);
SELECT ST_AsText(ST_MakePoint(3, 4));
SELECT ST_GeometryType('POINT(1 2)'::geometry);
SELECT GeometryType('LINESTRING(0 0,1 1)'::geometry);

-- SRID handling.
SELECT ST_SRID(ST_SetSRID(ST_MakePoint(1, 2), 4326));
SELECT ST_AsText(ST_SetSRID(ST_MakePoint(1, 2), 4326));

-- Exact planar distance (3-4-5 triangle) and the ST_DWithin predicate.
SELECT ST_Distance(ST_MakePoint(0, 0), ST_MakePoint(3, 4));
SELECT ST_DWithin(ST_MakePoint(0, 0), ST_MakePoint(3, 4), 5);
SELECT ST_DWithin(ST_MakePoint(0, 0), ST_MakePoint(3, 4), 4);

-- Topological predicates.
SELECT ST_Contains('POLYGON((0 0,0 10,10 10,10 0,0 0))'::geometry, 'POINT(5 5)'::geometry);
SELECT ST_Intersects('LINESTRING(0 0,10 10)'::geometry, 'LINESTRING(0 10,10 0)'::geometry);

-- The && bounding-box overlap operator.
SELECT 'POINT(0 0)'::geometry && 'POLYGON((-1 -1,-1 1,1 1,1 -1,-1 -1))'::geometry AS overlaps;

-- A spatial table with a GiST index, queried via && (index path through the
-- gateway). Counts are integers, so output is version-stable.
CREATE TABLE postgis_regress (id int PRIMARY KEY, geom geometry(Point, 4326));
INSERT INTO postgis_regress (id, geom) VALUES
	(1, ST_SetSRID(ST_MakePoint(0, 0), 4326)),
	(2, ST_SetSRID(ST_MakePoint(1, 1), 4326)),
	(3, ST_SetSRID(ST_MakePoint(50, 50), 4326));
CREATE INDEX postgis_regress_gix ON postgis_regress USING gist (geom);
SELECT count(*) FROM postgis_regress
	WHERE geom && ST_MakeEnvelope(-1, -1, 2, 2, 4326);
SELECT id FROM postgis_regress
	WHERE ST_DWithin(geom, ST_SetSRID(ST_MakePoint(0, 0), 4326), 2)
	ORDER BY id;
DROP TABLE postgis_regress;
