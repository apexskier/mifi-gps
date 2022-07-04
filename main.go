package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/adrianmo/go-nmea"
	_ "github.com/lib/pq"
)

type Http0_9ConnWrapper struct {
	net.Conn
	haveReadAny bool
}

func (c *Http0_9ConnWrapper) Read(b []byte) (int, error) {
	if c.haveReadAny {
		return c.Conn.Read(b)
	}
	c.haveReadAny = true
	// fake an http 1.1 connection to make the default go http client happier
	response := []byte("HTTP/1.1 200 OK\r\nConnection: keep-alive\r\nContent-Type: text/plain\r\n\r\n")
	copy(b, response)
	return len(response), nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type MifiNMEAData struct {
	// fields will be nil before initialization

	RMC *nmea.RMC
	GGA *nmea.GGA
	GSA *nmea.GSA
	GSV *nmea.GSV
	VTG *nmea.VTG

	m sync.Mutex
}

func (d *MifiNMEAData) Lock() {
	d.m.Lock()
}

func (d *MifiNMEAData) Unlock() {
	d.m.Unlock()
}

func (d *MifiNMEAData) Clear() {
	d.Lock()
	d.RMC = nil
	d.GGA = nil
	d.GSA = nil
	d.GSV = nil
	d.VTG = nil
	d.Unlock()
}

var funcMap = template.FuncMap{
	"gps": nmea.FormatGPS,
	"dms": nmea.FormatDMS,
}

//go:embed index.html
var rawIndexTemplate string
var indexTemplate = template.Must(template.New("index.html").Funcs(funcMap).Parse(rawIndexTemplate))

type templateData struct {
	MapsAPIKey string
	Data       *MifiNMEAData
}

var ErrNoDataToLog = fmt.Errorf("no data to log")

type queuedOp struct {
	query string
	args  []interface{}
}

func main() {
	connStr := os.Getenv("MIFI_GPS_DBCONNSTR")
	if connStr == "" {
		panic("missing db connection string in env var MIFI_GPS_DBCONNSTR")
	}

	mapsAPIKey := os.Getenv("MIFI_GPS_MAPSAPIKEY")
	if connStr == "" {
		panic("missing maps api key in env var MIFI_GPS_MAPSAPIKEY")
	}

	data := MifiNMEAData{}
	tmplData := templateData{
		MapsAPIKey: mapsAPIKey,
		Data:       &data,
	}

	var wg sync.WaitGroup

	http.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		data.Lock()
		defer data.Unlock()
		if err := indexTemplate.Execute(rw, tmplData); err != nil {
			log.Printf("error rendering web page: %s\n", err)
		}
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Println("starting web UI")
		err := http.ListenAndServe("0.0.0.0:8080", nil)
		if err != nil {
			panic(err)
		}
	}()

	queue := make([]queuedOp, 0)

	queueLocation := func() error {
		data.Lock()
		defer data.Unlock()
		// try to add a new piece of data
		if data.RMC == nil || data.GGA == nil {
			return ErrNoDataToLog
		}
		log.Print("queuing location")
		t, err := time.Parse("02/01/06T15:04:05.9999", fmt.Sprintf("%sT%s", data.RMC.Date.String(), data.RMC.Time.String()))
		if err != nil {
			return fmt.Errorf("failed to parse RMC date time: %w", err)
		}
		queue = append(queue, queuedOp{
			query: `INSERT INTO gps_logs(logged_at, gps_timestamp, gps_geometry, gps_speed, gps_course) VALUES($1, $2, ST_GeographyFromText($3), $4, $5)`,
			args: []interface{}{
				time.Now(),
				t,
				fmt.Sprintf("SRID=4326;POINTZ(%f %f %f)", data.RMC.Longitude, data.RMC.Latitude, data.GGA.Altitude),
				data.RMC.Speed,
				data.RMC.Course,
			},
		})
		// don't infinitely take up memory
		queue = queue[:max(len(queue)-100, len(queue))]
		return nil
	}

	pushToDB := func(db *sql.DB) error {
		log.Printf("pushing GPS data (%d in queue)\n", len(queue))
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("failed to start db txn: %w", err)
		}
		for len(queue) != 0 {
			// pop the first item in the queue, work through list
			op := queue[0]
			if _, err := tx.Exec(op.query, op.args...); err != nil {
				return fmt.Errorf("failed to insert to DB: %w", err)
			}
			queue = queue[1:]
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit db txn: %w", err)
		}
		return nil
	}

	wg.Add(1)
	go func() {
		db, err := sql.Open("postgres", connStr)
		if err != nil {
			panic(err)
		}
		log.Println("opened DB connection")
		defer db.Close()
		for {
			if err := pushToDB(db); err != nil {
				log.Printf("error pushing GPS data: %v\n", err)
			}
			time.Sleep(time.Minute * 5)
		}
	}()

	wg.Add(1)
	go func() {
		time.Sleep(time.Second * 10)
		for {
			if err := queueLocation(); err != nil {
				if errors.Is(err, ErrNoDataToLog) {
					log.Println("skipped queuing, no data")
				} else {
					log.Printf("error queuing location: %v\n", err)
				}
			}
			time.Sleep(time.Minute * 15)
		}
	}()

	parseGPS := func(line []byte) error {
		s, err := nmea.Parse(string(line))
		if err != nil {
			return fmt.Errorf("failed to parse nmea line: %w", err)
		}
		data.Lock()
		defer data.Unlock()
		switch s.DataType() {
		case nmea.TypeRMC:
			// Recommended Minimum Specific GPS/Transit data
			m := s.(nmea.RMC)
			data.RMC = &m
			// log.Println("parsed RMC	")
		case nmea.TypeGGA:
			// GPS Positioning System Fix Data
			m := s.(nmea.GGA)
			data.GGA = &m
			// log.Println("parsed GGA")
		case nmea.TypeGSA:
			// GPS DOP and active satellites
			m := s.(nmea.GSA)
			data.GSA = &m
			// log.Println("parsed GSA")
		case nmea.TypeGSV:
			// GPS Satellites in view
			m := s.(nmea.GSV)
			data.GSV = &m
			// log.Println("parsed GSV")
		case nmea.TypeVTG:
			// Track Made Good and Ground Speed
			m := s.(nmea.VTG)
			data.VTG = &m
			// log.Println("parsed VTG")
		default:
			return fmt.Errorf("unexpected nmea data type: %s", s.DataType())
		}
		return nil
	}

	getGPS := func() error {
		http0_9Transport := &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				realConn, err := net.Dial(network, addr)
				if err != nil {
					return nil, err
				}
				return &Http0_9ConnWrapper{Conn: realConn}, nil
			},
		}

		ctx := context.Background()
		server := "http://192.168.1.1:11010"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server, nil)
		if err != nil {
			return err
		}
		log.Println("connected to GPS HTTP stream")
		client := &http.Client{
			Transport: http0_9Transport,
		}
		res, err := client.Do(req)
		if err != nil {
			return err
		}

		reader := bufio.NewReader(res.Body)
		for {
			line, _, err := reader.ReadLine()
			if errors.Is(err, io.EOF) {
				return errors.New("reached end of connection to mifi")
			}
			if err != nil {
				return err
			}
			line = bytes.Trim(line, "\x00")
			if string(line) == "" {
				continue
			}
			if err := parseGPS(line); err != nil {
				return fmt.Errorf("failed to parse gps line: %w", err)
			}
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if err := getGPS(); err != nil {
				log.Println("error getting GPS", err)
				data.Clear()
			}
			time.Sleep(time.Minute)
		}
	}()

	wg.Wait()
}
