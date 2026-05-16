package main

import (
	"cmp"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ÖBB Scotty JSONP response structures
type scottyResponse struct {
	StationName string    `json:"stationName"`
	Journey     []journey `json:"journey"`
}

type journey struct {
	TI string          `json:"ti"` // scheduled time "HH:MM"
	PR string          `json:"pr"` // line/product e.g. "REX 1", "Bus 14A"
	ST string          `json:"st"` // direction/terminal station
	RT json.RawMessage `json:"rt"` // real-time info, can be false or object
}

type rtInfo struct {
	Status *string `json:"status"` // null or "Ausfall"
	DLT    string  `json:"dlt"`    // real-time departure time "HH:MM"
}

type stationEntry struct {
	ID             string
	AdditionalTime int
	Directions     []string
}

type departure struct {
	Time      string
	Line      string
	Station   string
	Direction string
	SortKey   int // minutes from now
}

const (
	defaultNumJourneys    = 6
	defaultTotal          = 12
	defaultProductsFilter = "1011111111011"
	scottyBaseURL         = "https://fahrplan.oebb.at/bin/stboard.exe/dn"
	httpTimeout           = 12 * time.Second
)

var httpClient = &http.Client{Timeout: httpTimeout}

func main() {
	port := cmp.Or(os.Getenv("PORT"), "80")

	mux := http.NewServeMux()
	mux.HandleFunc("/departures.csv", handleDeparturesCSV)

	log.Printf("oebb-monitor listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func handleDeparturesCSV(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	stationsRaw := q.Get("stations")
	if stationsRaw == "" {
		http.Error(w, "stations parameter required", http.StatusBadRequest)
		return
	}

	numJourneys := parseIntOr(q.Get("num_journeys"), defaultNumJourneys)
	total := parseIntOr(q.Get("total"), defaultTotal)
	productsFilter := cmp.Or(q.Get("products_filter"), defaultProductsFilter)

	entries, err := parseStations(stationsRaw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	now := time.Now()
	if loc := loadLocation(); loc != nil {
		now = now.In(loc)
	}

	var mu sync.Mutex
	var allDepartures []departure
	seen := make(map[string]bool)

	var wg sync.WaitGroup
	for _, entry := range entries {
		entry := entry
		dirs := entry.Directions
		if len(dirs) == 0 {
			dirs = []string{""}
		}
		for _, dir := range dirs {
			wg.Add(1)
			go func(stationID, directionID string) {
				defer wg.Done()
				deps, err := fetchStation(stationID, directionID, numJourneys, entry.AdditionalTime, productsFilter, now)
				if err != nil {
					log.Printf("error fetching station %s dir %s: %v", stationID, directionID, err)
					return
				}
				mu.Lock()
				for _, d := range deps {
					key := d.Time + "|" + d.Line + "|" + d.Direction
					if !seen[key] {
						seen[key] = true
						allDepartures = append(allDepartures, d)
					}
				}
				mu.Unlock()
			}(entry.ID, dir)
		}
	}
	wg.Wait()

	sort.Slice(allDepartures, func(i, j int) bool {
		return allDepartures[i].SortKey < allDepartures[j].SortKey
	})

	if len(allDepartures) > total {
		allDepartures = allDepartures[:total]
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")

	cw := csv.NewWriter(w)

	// First row: current time in the Zeit column, then column headers
	cw.Write([]string{
		fmt.Sprintf("%02d:%02d", now.Hour(), now.Minute()),
		"Linie",
		"Von",
		"Richtung",
	})

	for _, d := range allDepartures {
		cw.Write([]string{d.Time, d.Line, shortenName(d.Station), shortenName(d.Direction)})
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		log.Printf("csv write error: %v", err)
	}
}

func parseStations(raw string) ([]stationEntry, error) {
	parts := strings.Split(raw, ",")
	entries := make([]stationEntry, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		stationAndTime := strings.SplitN(p, "@", 2)
		if len(stationAndTime) != 2 {
			return nil, fmt.Errorf("station selector %q must use stationId@additionalTime[:dir1:dir2...]", p)
		}
		stationID := strings.TrimSpace(stationAndTime[0])
		if stationID == "" {
			return nil, fmt.Errorf("station selector %q is missing a station id", p)
		}
		timeAndDirs := strings.Split(stationAndTime[1], ":")
		if len(timeAndDirs) == 0 || strings.TrimSpace(timeAndDirs[0]) == "" {
			return nil, fmt.Errorf("station selector %q is missing additionalTime", p)
		}
		additionalTime, err := strconv.Atoi(strings.TrimSpace(timeAndDirs[0]))
		if err != nil {
			return nil, fmt.Errorf("station selector %q has invalid additionalTime: %w", p, err)
		}
		entry := stationEntry{ID: stationID, AdditionalTime: additionalTime}
		for _, d := range timeAndDirs[1:] {
			d = strings.TrimSpace(d)
			if d != "" {
				entry.Directions = append(entry.Directions, d)
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func fetchStation(stationID, directionID string, numJourneys, additionalTime int, productsFilter string, now time.Time) ([]departure, error) {
	url := fmt.Sprintf(
		"%s?L=vs_scotty.vs_liveticker&tickerID=dep&start=yes&eqstops=false"+
			"&evaId=%s&dirInput=%s&showJourneys=%d&maxJourneys=%d"+
			"&additionalTime=%d&productsFilter=%s&boardType=dep&outputMode=tickerDataOnly",
		scottyBaseURL, stationID, directionID, numJourneys, numJourneys,
		additionalTime, productsFilter,
	)

	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// Strip JSONP wrapper: "journeysObj = {...}\n"
	jsonStr := string(body)
	if idx := strings.Index(jsonStr, "{"); idx >= 0 {
		jsonStr = jsonStr[idx:]
	}
	jsonStr = strings.TrimRight(jsonStr, " \t\r\n;")

	var scotty scottyResponse
	if err := json.Unmarshal([]byte(jsonStr), &scotty); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w (body prefix: %q)", err, truncate(string(body), 200))
	}

	nowMinutes := now.Hour()*60 + now.Minute()

	var deps []departure
	for _, j := range scotty.Journey {
		rt, hasRT := parseRT(j.RT)
		if hasRT && rt.Status != nil && *rt.Status == "Ausfall" {
			continue
		}

		scheduledTime := j.TI
		actualTime := scheduledTime
		if hasRT && rt.DLT != "" {
			actualTime = rt.DLT
		}

		deps = append(deps, departure{
			Time:      actualTime,
			Line:      cleanHTML(j.PR),
			Station:   cleanHTML(scotty.StationName),
			Direction: cleanHTML(j.ST),
			SortKey:   minutesFromNow(timeToMinutes(actualTime), nowMinutes),
		})
	}

	return deps, nil
}

func parseRT(raw json.RawMessage) (rtInfo, bool) {
	if len(raw) == 0 {
		return rtInfo{}, false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "false" || trimmed == "null" {
		return rtInfo{}, false
	}
	var info rtInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return rtInfo{}, false
	}
	return info, true
}

func timeToMinutes(hhmm string) int {
	parts := strings.SplitN(hhmm, ":", 2)
	if len(parts) != 2 {
		return 0
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return h*60 + m
}

func minutesFromNow(absoluteMinutes, nowMinutes int) int {
	diff := absoluteMinutes - nowMinutes
	if diff < -12*60 {
		diff += 24 * 60
	}
	if diff > 12*60 {
		diff -= 24 * 60
	}
	return diff
}

// cleanHTML strips HTML tags and decodes all HTML entities (named and numeric)
func cleanHTML(s string) string {
	var out strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			continue
		}
		if !inTag {
			out.WriteRune(r)
		}
	}
	return html.UnescapeString(out.String())
}

func shortenName(name string) string {
	r := strings.NewReplacer(
		"Wien ", "",
		"Matzleinsdorfer", "Matzl",
		"platz", "pl.",
		"Platz", "Pl.",
		"straße", "str",
		"strasse", "str",
		"Straße", "Str",
		"Strasse", "Str",
		"gasse", "g.",
		"Gasse", "G.",
		"Bahnhof", "Bhf",
		"Bahnhst", "Bst",
	)
	return r.Replace(name)
}

func loadLocation() *time.Location {
	tz := cmp.Or(os.Getenv("TZ"), "Europe/Vienna")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		log.Printf("warning: could not load timezone %q: %v", tz, err)
		return nil
	}
	return loc
}

func parseIntOr(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."

}
