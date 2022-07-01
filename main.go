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
	Data *MifiNMEAData
}

func main() {
	connStr := os.Getenv("MIFI_GPS_DBCONNSTR")
	if connStr == "" {
		panic("missing db connection string in env var MIFI_GPS_DBCONNSTR")
	}

	data := MifiNMEAData{}

	var wg sync.WaitGroup

	http.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		data.Lock()
		indexTemplate.Execute(rw, templateData{Data: &data})
		data.Unlock()
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

	wg.Add(1)
	go func() {
		db, err := sql.Open("postgres", connStr)
		if err != nil {
			panic(err)
		}
		log.Println("opened DB connection")
		defer db.Close()
		for {
			if data.RMC == nil {
				continue
			}
			t, err := time.Parse("02/01/06T15:04:05.9999", fmt.Sprintf("%sT%s", data.RMC.Date.String(), data.RMC.Time.String()))
			if err != nil {
				panic(err)
			}
			_, err = db.Exec(
				`INSERT INTO gps_logs(logged_at, gps_timestamp, gps_geometry, gps_speed, gps_course) VALUES($1, $2, ST_GeographyFromText($3), $4, $5)`,
				time.Now(),
				t,
				fmt.Sprintf("SRID=4326;POINTZ(%f %f %f)", data.RMC.Longitude, data.RMC.Latitude, data.GGA.Altitude),
				data.RMC.Speed,
				data.RMC.Course,
			)
			if err != nil {
				panic(err)
			}
			log.Println("logged GPS data to DB")
			time.Sleep(time.Minute * 15)
		}
	}()

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

			s, err := nmea.Parse(string(line))
			if err != nil {
				return fmt.Errorf("failed to parse nmea line: %w", err)
			}
			data.Lock()
			switch s.DataType() {
			case nmea.TypeRMC:
				// Recommended Minimum Specific GPS/Transit data
				m := s.(nmea.RMC)
				data.RMC = &m
			case nmea.TypeGGA:
				// GPS Positioning System Fix Data
				m := s.(nmea.GGA)
				data.GGA = &m
			case nmea.TypeGSA:
				// GPS DOP and active satellites
				m := s.(nmea.GSA)
				data.GSA = &m
			case nmea.TypeGSV:
				// GPS Satellites in view
				m := s.(nmea.GSV)
				data.GSV = &m
			case nmea.TypeVTG:
				// Track Made Good and Ground Speed
				m := s.(nmea.VTG)
				data.VTG = &m
			default:
				log.Printf("unexpected nmea data type: %s\n", s.DataType())
			}
			data.Unlock()
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
