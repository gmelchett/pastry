// SPDX-FileCopyrightText: 2023 Jonas Aaberg
//
// SPDX-License-Identifier: MIT

// pastry - a pastebin server for your home network
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
	"unicode/utf8"

	"gerace.dev/zipfs"
	"github.com/OpenPeeDeeP/xdg"
	"github.com/dustin/go-humanize"
)

//go:embed css/pico-master.zip
var picocssZipFile []byte

//go:embed tmpl/index.html
var indexTemplate string

//go:embed static/favicon.png
var favicon []byte

//go:embed static/pastry.png
var logo []byte

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
	buf := make([]byte, 1024*1024)

	if n, err := c.Read(buf); err == nil && n > 0 {
		if utf8.Valid(buf[:n]) {
			p.addText(string(buf[:n]))
		}
	}
}

func (p *pastry) handleReadPaste(c net.Conn) {
	defer c.Close()

	p.mutex.Lock()
	defer p.mutex.Unlock()

	buf := make([]byte, 1024*1024)
	c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

	n, err := c.Read(buf)

	if err != nil || n == 0 {
		c.Write([]byte(p.texts[len(p.texts)-1].Text))
		return
	}

	s := strings.ReplaceAll(string(buf[:n]), "\n", "")
	cmd := strings.Fields(s)
	if len(cmd) == 0 {
		return
	}

	toIdx := func() (int, error) {
		if len(cmd) == 1 {
			return len(p.texts) - 1, nil
		}

		if v, err := strconv.Atoi(cmd[1]); err == nil {
			if v >= 0 && v < len(p.texts) {
				return v, nil
			} else if v <= 0 && (len(p.texts)+v) >= 0 {
				return len(p.texts) + v, nil
			}
		}
		return 0, fmt.Errorf("Out of bounds")
	}

	switch cmd[0] {
	case "get":
		if i, err := toIdx(); err == nil {
			c.Write([]byte(p.texts[i].Text))
		}
	case "grep":
		var b bytes.Buffer
		_, m, _ := strings.Cut(s, "grep ")
		for i := range p.texts {
			for num, l := range strings.Split(p.texts[i].Text, "\n") {
				if idx := strings.Index(l, m); idx != -1 {
					when := humanize.Time(p.texts[i].When)
					pad := ""
					if len(when) < 20 {
						pad = strings.Repeat(" ", 20-len(when))
					}
					b.WriteString(fmt.Sprintf("#% 3d\t% 3d\t%s%s\t%s\n", i, num+1, when, pad, l))
				}
			}
		}
		c.Write(b.Bytes())
	case "list":
		var b bytes.Buffer

		for i := range p.texts {
			when := humanize.Time(p.texts[i].When)
			pad := ""
			if len(when) < 20 {
				pad = strings.Repeat(" ", 20-len(when))
			}
			b.WriteString(fmt.Sprintf("#% 3d\t%s%s\t%s\n", i, when, pad, strings.Trim(p.texts[i].Text, "\n")))

		}
		c.Write(b.Bytes())

	case "drop":
		if i, err := toIdx(); err == nil {
			p.texts = append(p.texts[:i], p.texts[i+1:]...)
		}
	default:
		c.Write([]byte("# Unknown command\n"))
	}
}

func faviconHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(favicon)
}

func logoHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(logo)
}

type htmlEntry struct {
	DateTime string
	Text     string
}

func (p *pastry) showPastry(w http.ResponseWriter, _ *http.Request) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

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
	mux.HandleFunc("/favicon.png", faviconHandler)
	mux.HandleFunc("/logo.png", logoHandler)

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
