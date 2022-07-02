package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/cridenour/go-postgis"
	_ "github.com/lib/pq"
)

func main() {
	ctx := context.Background()

	connStr := os.Getenv("CONN_STRING")
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	data, err := db.QueryContext(ctx, "SELECT logged_at, gps_timestamp, gps_geometry FROM gps_logs ORDER BY logged_at DESC LIMIT 1;")
	if err != nil {
		panic(err)
	}
	var t time.Time
	var t2 time.Time
	var gpsPoint postgis.PointZS
	if data.Next() {
		err = data.Scan(&t, &t2, &gpsPoint)
		if err != nil {
			panic(err)
		}
		fmt.Printf(`t: %s
t2: %s
x: %f
y: %f
z: %f
`, t.String(), t2.String(), gpsPoint.X, gpsPoint.Y, gpsPoint.Z)
	}
}
