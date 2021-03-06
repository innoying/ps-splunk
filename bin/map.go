package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Setup the loggers
var infoLogger = log.New(os.Stdout, "", log.Ldate|log.Ltime|log.Lshortfile)
var errorLogger = log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile)

// Holds the wait group before exiting
var wg sync.WaitGroup

// Define a thread safe cache of hosts we've already looked up
var cache = struct {
	sync.RWMutex
	m map[string]bool
}{m: make(map[string]bool)}

// Link holds information on how a host is linked with origin at timestamp
type Link struct {
	Host   string `json:"host"`
	Origin string `json:"origin"`
}

// The output queues
var links = make(chan []byte, 10000000)
var summaries = make(chan []byte, 10000000)
var results = make(chan []byte, 10000000)

// Test defines structures for tests
type Test struct {
	LastUpdated   int    `json:"last_updated"`
	DestinationIP string `json:"destination_ip"`
	SourceIP      string `json:"source_ip"`
}

// Global http client
var client = http.Client{
	// Timeout requests after 10 seconds
	Timeout: time.Duration(10 * time.Second),
}

// Adds an host to the queue and cache if not already in cache
func dedup(host string, origin string) {
	// Convert IPv6
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	// Shitty speed optimization
	links <- []byte("{\"address\":\"" + host + "\",\"origin\":\"" + origin + "\"}\n")
	cache.RLock()
	_, ok := cache.m[host]
	cache.RUnlock()
	if !ok {
		cache.Lock()
		cache.m[host] = true
		cache.Unlock()
		// Run the worker in the background
		go worker(host)
	}
}

// Handles a job
func worker(host string) {
	// Request the summary for that host
	infoLogger.Printf("Getting summary for: %s\n", host)
	resp, err := client.Get("http://" + host + "/toolkit/services/host.cgi?method=get_summary")
	if err != nil {
		errorLogger.Println(err)
		return
	}
	// If it wasn't a json response skip this host
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		return
	}
	// Read the response
	summary, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		errorLogger.Println(err)
		return
	}
	// Add to summaries output queue
	summaries <- append(summary, byte('\n'))
	// Get the test list
	infoLogger.Printf("Getting test list for: %s\n", host)
	resp, err = client.Get("http://" + host + "/perfsonar-graphs/graphData.cgi?action=test_list&url=http%3A%2F%2Flocalhost%2Fesmond%2Fperfsonar%2Farchive%2F")
	if err != nil {
		errorLogger.Println(err)
		return
	}
	// If it wasn't a json response skip this host
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/json") {
		return
	}
	// Make a object for the tests to be stored in
	tests := []Test{}
	// Parse the body
	err = json.NewDecoder(resp.Body).Decode(&tests)
	if err != nil {
		return
	}
	// For each test
	for _, test := range tests {
		// Queue both the src and dst
		dedup(test.DestinationIP, host)
		dedup(test.SourceIP, host)
	}
	// Get the test results
	infoLogger.Printf("Getting test results for: %s\n", host)
	resp, err = client.Get("http://" + host + "/perfsonar-graphs/graphData.cgi?action=tests&url=http%3A%2F%2Flocalhost%2Fesmond%2Fperfsonar%2Farchive%2F")
	if err != nil {
		errorLogger.Println(err)
		return
	}
	// If it wasn't a json response skip this host
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/json") {
		return
	}
	// Read the testResults
	var testResults []json.RawMessage
	// Parse the body
	err = json.NewDecoder(resp.Body).Decode(&testResults)
	if err != nil {
		errorLogger.Println(err)
		return
	}
	// Loop each result
	for _, testResult := range testResults {
		// Add to testResults output queue
		results <- append(testResult, byte('\n'))
	}
}

// Get the startup time of the program
var startTime = time.Now().Format(time.UnixDate)

// Log writer takes a channel and writes it to a file
func logWriter(suffix string, logs <-chan []byte) {
	// Generate the filename
	filename := startTime + "-" + suffix + ".json"
	// Open the log file
	logFile, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		errorLogger.Fatal(err)
	}
	defer logFile.Close()
	// As logs come in write it followed by a newline
	for log := range logs {
		_, err = logFile.Write(log)
		if err != nil {
			errorLogger.Fatal(err)
		}
	}
}

// Looks up a given string until it is resolved to an IP then queues it
func getIP(host string, origin string) {
	// Bail if none provided
	if host == "" {
		return
	}
	// Try to parse it as an IP, if fails look it up
	if addr := net.ParseIP(host); addr == nil {
		// Try to lookup the host
		addrs, err := net.LookupHost(host)
		if err != nil {
			errorLogger.Println(err)
			return
		}
		for _, addr := range addrs {
			getIP(addr, origin)
		}
	} else {
		// Add to results and return
		dedup(addr.String(), origin)
	}
}

// Process the cache
func processCache(records [][]string, origin string) {
	defer wg.Done()
	// Loop each record
	for _, record := range records {
		// Parse the url
		url, err := url.Parse(record[0])
		if err != nil {
			errorLogger.Println(err)
			continue
		}
		// If there was a host/port
		if url.Host != "" {
			// Extract just the host
			shost, _, err := net.SplitHostPort(url.Host)
			if err != nil {
				errorLogger.Println(err)
				continue
			}
			// Resolve to an IP and queue
			getIP(shost, origin)
		}
	}
}

// Reads a given cache file
func getCache(cache string) {
	defer wg.Done()
	// Get the main lookup file
	resp, err := client.Get(cache)
	if err != nil {
		errorLogger.Fatal(err)
	}
	defer resp.Body.Close()
	// Read the entire body into memory first
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		errorLogger.Fatal(err)
	}
	// Un g-zip the tarball
	gzf, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		errorLogger.Fatal(err)
	}
	// Create a tar reader
	tarReader := tar.NewReader(gzf)
	// Loop forever
	for {
		// Read the next file
		header, err := tarReader.Next()
		// If at end of tar, bail else bail with the error
		if err == io.EOF {
			break
		} else if err != nil {
			errorLogger.Fatal(err)
		}
		// Depending on the type of entry
		switch header.Typeflag {
		case tar.TypeReg:
			// Load it as a PSV file
			r := csv.NewReader(tarReader)
			r.Comma = '|'
			r.LazyQuotes = true
			records, err := r.ReadAll()
			if err != nil {
				errorLogger.Println(err)
				continue
			}
			infoLogger.Printf("Processing cache file: %s\n", header.Name)
			wg.Add(1)
			go processCache(records, "cache,"+header.Name+","+cache)
		case tar.TypeDir:
			continue
		default:
			break
		}
	}
}

func getCaches(hints string) {
	// Get the hints file
	resp, err := client.Get(hints)
	if err != nil {
		errorLogger.Fatal(err)
	}
	// Create a scanner for the body
	scanner := bufio.NewScanner(resp.Body)
	// For each newline
	for scanner.Scan() {
		// Get the information on that cache
		wg.Add(1)
		go getCache(scanner.Text())
	}
	resp.Body.Close()
	if err := scanner.Err(); err != nil {
		errorLogger.Fatal(err)
	}
}

// Entry point
func main() {
	// Spawn the log writers
	go logWriter("link", links)
	go logWriter("summary", summaries)
	go logWriter("results", results)
	// Get the caches to start the process
	getCaches("http://www.perfsonar.net/ls.cache.hints")
	// Wait for all jobs to finish before exiting
	wg.Wait()
}
