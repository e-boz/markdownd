/*
* The MIT License (MIT)
*
* Copyright (c) 2017  aerth <aerth@riseup.net>
*
* Permission is hereby granted, free of charge, to any person obtaining a copy
* of this software and associated documentation files (the "Software"), to deal
* in the Software without restriction, including without limitation the rights
* to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
* copies of the Software, and to permit persons to whom the Software is
* furnished to do so, subject to the following conditions:
*
* The above copyright notice and this permission notice shall be included in all
* copies or substantial portions of the Software.
*
* THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
* IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
* FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
* AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
* LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
* OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
* SOFTWARE.
 */

// Command markdownd serves markdown, static, and html files.
package main

import (
	"flag"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/russross/blackfriday"
)

var (
	addr      = flag.String("http", ":8080", "address to listen on")
	logfile   = flag.String("log", os.Stderr.Name(), "redirect logs to this file")
	indexPage = flag.String("index", "index.md", "page to use for paths ending in '/'")
)

type Server struct {
	Root       http.FileSystem
	RootString string
}

const version = "0.0.6"
const sig = "[markdownd v" + version + "]\nhttps://github.com/aerth/markdownd"
const serverheader = "markdownd/" + version

func init() {
	flag.Usage = func() {
		println(usage)
		println("FLAGS")
		flag.PrintDefaults()
	}
	rand.Seed(time.Now().UnixNano())
}

const usage = `
USAGE

markdownd [flags] [directory]

EXAMPLES

Serve current directory on port 8080, log to stderr
	markdownd -log /dev/stderr -http 127.0.0.1:8080 .

Serve 'docs' directory on port 8081, log to 'md.log'
	markdownd -log md.log -http :8081`

func main() {
	println(sig)
	// need only 1 argument
	flag.Parse()
	args := flag.Args()
	if len(args) != 1 {
		println(usage)
		os.Exit(111)
		return
	}

	logger := log.New(os.Stderr, "", 0)

	// get absolute path of flag.Arg(0)
	dir := flag.Arg(0)
	dir = prepareDirectory(dir)
	// new server
	srv := &Server{
		Root:       http.Dir(dir),
		RootString: dir,
	}

	println("serving filesystem:", dir)

	if *logfile != os.Stderr.Name() {
		func(){
			f, err := os.OpenFile(*logfile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0660)
			if err != nil {
				logger.Fatalf("cant open log file: %s", err)
			}
			logger.SetOutput(f)
		}()
	}

	println("log output:", *logfile)

	go func() { <-time.After(time.Second); println("listening:", *addr) }()

	// create a http server
	server := &http.Server{
		Addr: *addr,
		Handler: srv,
		ErrorLog: logger,
	}
	server.SetKeepAlivesEnabled(false)

	// start serving
	err := server.ListenAndServe()

	// always non-nil
	log.Println(err)
	return
}

func rfid() string {
	return strconv.Itoa(rand.Int())
}

func (s Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body.Close()

	// all we want is GET
	if r.Method != "GET" {
		log.Println("bad method:", r.RemoteAddr, r.Method, r.URL.Path, r.UserAgent())
		http.NotFound(w, r)
		return
	}

	// deny requests containing '../'
	if strings.Contains(r.URL.Path, "../") {
		log.Println("bad path:", r.RemoteAddr, r.Method, r.URL.Path, r.UserAgent())
		http.NotFound(w, r)
		return
	}

	t1 := time.Now()

	// Add Server header
	w.Header().Add("Server", serverheader)

	// Prevent page from being displayed in an iframe
	w.Header().Add("X-Frame-Options", "DENY")

	// generate unique request id
	requestid := rfid()

	// abs is not absolute yet
	abs := r.URL.Path[1:] // remove slash

	if abs == "" {
		abs = *indexPage
	}

	// / suffix, add *index.Page
	if strings.HasSuffix(abs, "/") {
		abs += "index.md"
	}

	// prepend root directory to filesrc
	abs = s.RootString + abs

	// log now that we have filename
	log.Println(requestid, r.RemoteAddr, r.Method, r.URL.Path, "->", abs)

	// log how long this takes
	defer log.Println(requestid, "closed after", time.Now().Sub(t1))

	// get absolute path of requested file (could not exist)
	abs, err := filepath.Abs(abs)
	if err != nil {
		log.Println(requestid, "error resolving absolute path:", err)
		http.NotFound(w, r)
		return
	}

	// .html suffix
	if strings.HasSuffix(abs, ".html") {
		trymd := strings.TrimSuffix(abs, ".html") + ".md"
		_, err := os.Open(trymd)
		if err == nil {
			log.Println(requestid, abs, "->", trymd)
			abs = trymd
		}
	}



	// check if exists, or give 404
	_, err = os.Open(abs)
	if err != nil {
		if strings.Contains(err.Error(), "no such file") {
			log.Println(requestid, "404", abs)
			http.NotFound(w, r)
			return
		}

		// probably permissions
		log.Println(requestid, "error opening file:", err, abs)
		http.NotFound(w, r)
		return
	}

	// check if symlink ( to avoid /proc/self/root style attacks )
	if !fileisgood(abs) {
		log.Printf("%s error: %q is symlink. serving 404", requestid, abs)
		http.NotFound(w, r)
		return
	}

	// above, we checked for abs vs symlink resolved,
	// here lets check if they have the special prefix of "s.Root"
	// probably redundant.
	if !strings.HasPrefix(abs, s.RootString) {
		log.Println(requestid, "bad path", abs, "doesnt have prefix:", s.RootString)
		http.NotFound(w, r)
		return
	}

	// read bytes (for detecting content type )
	b, err := ioutil.ReadFile(abs)
	if err != nil {
		log.Printf("%s error reading file: %q", requestid, abs)
		http.NotFound(w, r)
		return
	}

	// detect content type and encoding
	ct := http.DetectContentType(b)

	// serve raw html if exists
	if strings.HasSuffix(abs, ".html") || strings.HasPrefix(ct, "text/html") {
		
		log.Println(requestid, "serving raw html:", abs)
		w.Header().Add("Content-Type", "text/html")
		w.Write(b)
		return
	}

	// probably markdown
	if strings.HasSuffix(abs, ".md") && strings.HasPrefix(ct, "text/plain") {
		if strings.Contains(r.URL.RawQuery, "raw") {
			log.Println(requestid, "raw markdown request:", abs)
			w.Write(b)
			return
		}
		log.Println(requestid, "serving markdown:", abs)
		w.Write(blackfriday.MarkdownCommon(b))
		return
	}

	// fallthrough with http.ServeFile
	log.Printf("%s serving %s: %s", requestid, ct, abs)

	http.ServeFile(w, r, abs)
}

// fileisgood returns false if symlink
// comparing absolute vs resolved path is apparently quick and effective
func fileisgood(abs string) bool {
	if abs == "" {
		return false
	}

	var err error
	if !filepath.IsAbs(abs) {
		abs, err = filepath.Abs(abs)
	}

	if err != nil {
		println(err.Error())
		return false
	}

	realpath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		println(err.Error())
		return false
	}
	return realpath == abs
}

// prepare root filesystem directory
func prepareDirectory(dir string) string {
	if dir == "." {
		dir += "/"
	}
	var err error
	dir, err = filepath.Abs(dir)
	if err != nil {
		println(err.Error())
		os.Exit(111)
		return err.Error()
	}

	// add trailing slash
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}

	return dir
}
