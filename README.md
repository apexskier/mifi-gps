Stores a history of GPS locations from a Inseego 5G MiFi M2000 device in a remote Postgres database.

Setup:

1. Set up the database with the [setup script](./db.psql).
2. Build the binary `go build .`
3. Run the binary with `./mifi-gps`, with the following environment variables set
    * `MIFI_GPS_DBCONNSTR` set to your DB connection string (this'll have address and credentials)
    * `MIFI_GPS_MAPSAPIKEY` set to a google static maps api key

A web server will be exposed at http://0.0.0.0:8080. Protect it as you like.

The Mifi is expected to be available at http://192.168.1.1:11010 (that's the standard port, and standard IP if you're directly connected to it).
