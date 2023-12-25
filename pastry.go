// SPDX-FileCopyrightText: 2023 Jonas Aaberg
//
// SPDX-License-Identifier: MIT

package main

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/gob"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gerace.dev/zipfs"
	"github.com/OpenPeeDeeP/xdg"
	"github.com/dustin/go-humanize"
)

//go:embed css/pico-master.zip
var picocssZipFile []byte

//go:embed tmpl/index.html
var indexTemplate string

type entry struct {
	Text string
	When time.Time
}

type pastry struct {
	mutex     sync.Mutex
	texts     []*entry
	tmpl      *template.Template
	cacheFile string
}

func (p *pastry) addText(text string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.texts = append(p.texts, &entry{Text: text, When: time.Now()})

	if f, err := os.Create(p.cacheFile); err == nil {
		gob.NewEncoder(f).Encode(p.texts)
		f.Close()
	}
}

func (p *pastry) handleWritePaste(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 128*1024)

	if n, err := c.Read(buf); err == nil && n > 0 {
		p.addText(string(buf[:n]))
	}
}

func (p *pastry) handleReadPaste(c net.Conn) {
	defer c.Close()
	var b bytes.Buffer

	start := 0

	buf := make([]byte, 128*1024)
	c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

	if n, err := c.Read(buf); err == nil && n > 0 {
		s := strings.TrimSpace(strings.ReplaceAll(string(buf[:n]), "\n", ""))
		if v, err := strconv.ParseUint(s, 0, 64); err == nil {
			if int(v) < len(p.texts) {
				start = len(p.texts) - int(v)
			}
		} else {
			fmt.Printf("Failed to parse number: '%s'", s)
		}

	} else if len(p.texts) > 10 {
		start = len(p.texts) - 10
	}

	for i := start; i < len(p.texts); i++ {
		b.WriteString(fmt.Sprintf(" * Entry: %d - %s\n%s\n", i, humanize.Time(p.texts[i].When), p.texts[i].Text))
	}

	c.Write(b.Bytes())

}

type htmlEntry struct {
	DateTime string
	Text     string
}

func (p *pastry) showPastry(w http.ResponseWriter, _ *http.Request) {
	h := make([]htmlEntry, 0, len(p.texts))

	for i := len(p.texts) - 1; i >= 0; i-- {
		h = append(h, htmlEntry{DateTime: humanize.Time(p.texts[i].When), Text: p.texts[i].Text})
	}

	p.tmpl.Execute(w, h)
}

func (p *pastry) paste(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		r.ParseForm()
		p.addText(r.Form["text"][0])
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func createDir(dir string) error {
	if stat, err := os.Stat(dir); err != nil || !stat.IsDir() {
		return os.MkdirAll(dir, 0755)
	}
	return nil
}

func main() {

	picocssZipReader, err := zip.NewReader(bytes.NewReader(picocssZipFile), int64(len(picocssZipFile)))
	if err != nil {
		log.Fatalf("pico-master.zip is faulty: %v", err)
	}

	picocssZipFs, err := zipfs.NewZipFileSystem(picocssZipReader)
	if err != nil {
		log.Fatalf("zipfs creation failure: %v", err)
	}
	p := pastry{}

	p.tmpl = template.Must(template.New("tmpl").Parse(indexTemplate))

	xdg := xdg.New("gmelchett", "pastry")
	if err = createDir(xdg.CacheHome()); err != nil {
		log.Fatalf("Failed to create cache directory: %v", err)
	}
	p.cacheFile = filepath.Join(xdg.CacheHome(), "pastes.gob")

	if f, err := os.Open(p.cacheFile); err == nil {
		gob.NewDecoder(f).Decode(&p.texts)
		f.Close()
	}

	writePastePort, err := net.Listen("tcp", ":9181")
	if err != nil {
		log.Fatalf("Failed to listen to write paste port: %v", err)
		return
	}
	defer writePastePort.Close()

	readPastePort, err := net.Listen("tcp", ":9182")
	if err != nil {
		log.Fatalf("Failed to listen to read paste port: %v", err)
		return
	}
	defer readPastePort.Close()

	mux := http.NewServeMux()

	mux.HandleFunc("/", p.showPastry)
	mux.Handle("/css/", http.StripPrefix("/css/", http.FileServer(picocssZipFs)))
	mux.HandleFunc("/paste", p.paste)

	go http.ListenAndServe(":9180", mux)

	go func() {
		for {
			if c, err := writePastePort.Accept(); err == nil {
				go p.handleWritePaste(c)
			} else {
				log.Fatalf("Accept failed: %v", err)
			}
		}
	}()

	for {
		if c, err := readPastePort.Accept(); err == nil {
			go p.handleReadPaste(c)
		} else {
			log.Fatalf("Accept failed: %v", err)
		}
	}
}
