package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	url := flag.String("url", os.Args[1], "target url")
	duration := flag.Duration("duration", 30*time.Minute, "duration of download")
	flag.Parse()

	out := os.Stdout

	parser := newInternetRadioUKParser()
	name, stream, err := parser.Parse(*url)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(out, "found stream: %s at %s\n", name, stream)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		done := make(chan os.Signal)
		signal.Notify(done, syscall.SIGTERM, syscall.SIGINT, os.Kill, os.Interrupt)
		<-done
		cancel()
		fmt.Fprint(out, "bye...\n")
	}()

	downloader := newDownloader(out)
	f, err := os.Create(fmt.Sprintf("%s.mp3", name))
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := downloader.Download(ctx, f, stream, *duration); err != nil {
		panic(err)
	}
}

var errStreamNotFound = errors.New("stream URL not found")

type internetRadioUKParser struct {
	c *http.Client
}

func newInternetRadioUKParser() *internetRadioUKParser {
	return &internetRadioUKParser{
		c: &http.Client{},
	}
}

func (d *internetRadioUKParser) findStation(url string) (string, error) {
	if url == "http://www.internetradiouk.com" {
		return "https://api.webrad.io/data/streams/42/bbc-radio-1", nil
	}
	prefix := "http://www.internetradiouk.com/#"
	if strings.HasPrefix(url, prefix) {
		return "https://api.webrad.io/data/streams/42/" + url[len(prefix):], nil
	}

	return "", errStreamNotFound
}

// Parse parse the given page and return the streaming URL with station name
func (d *internetRadioUKParser) Parse(url string) (name string, stream string, err error) {
	stationURL, err := d.findStation(url)
	if err != nil {
		return "", "", err
	}
	res, err := d.c.Get(stationURL)
	if err != nil {
		return "", "", fmt.Errorf("failed to get station from %s, err: %v", stationURL, err)
	}
	defer res.Body.Close()
	var s struct {
		Station struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"station"`
		Streams []struct {
			ID          int    `json:"id"`
			IsContainer bool   `json:"isContainer"`
			MediaType   string `json:"mediaType"`
			Mime        string `json:"mime"`
			URL         string `json:"url"`
		} `json:"streams"`
	}
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		return "", "", fmt.Errorf("failed to decode station result, err: %v", err)
	}
	for _, str := range s.Streams {
		if strings.HasPrefix(strings.ToLower(str.Mime), "audio") && !str.IsContainer {
			return s.Station.Name, str.URL, nil
		}
	}
	return "", "", errStreamNotFound
}

type logger = io.Writer

type downloader struct {
	c   *http.Client
	log logger
}

func newDownloader(l logger) *downloader {
	if l == nil {
		l = os.Stdout
	}
	return &downloader{
		c:   &http.Client{},
		log: l,
	}
}

func (d *downloader) Download(ctx context.Context, w io.Writer, url string, duration time.Duration) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := d.c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	pp := newProgressPrinter(d.log)
	mw := io.MultiWriter(w, pp)

	var wg sync.WaitGroup
	wg.Add(1)
	errChan := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	pp.Start()
	defer pp.Stop()
	go func() {
		defer wg.Done()
		if _, err := Copy(ctx, mw, res.Body); err != nil && err != context.Canceled {
			fmt.Fprintf(d.log, "error while streaming: %v\n", err)
			errChan <- err
		}
		errChan <- nil
	}()
	select {
	case <-time.After(duration):
		cancel()
	case <-ctx.Done():
		cancel()
	}
	wg.Wait()
	return <-errChan
}

type progressPrinter struct {
	writer io.Writer
	total  int64
	start  time.Time
}

func newProgressPrinter(w io.Writer) *progressPrinter {
	return &progressPrinter{
		writer: w,
	}
}

func (p *progressPrinter) Start() {
	p.start = time.Now()
	fmt.Fprintf(p.writer, "downloading started\n")
}

func (p *progressPrinter) progressKB() float64 {
	return float64(p.total) / float64(1024)
}

func (p *progressPrinter) Write(b []byte) (int, error) {
	p.total += int64(len(b))
	fmt.Fprintf(p.writer, "downloaded: %.2fKB, time spent: %v\n", p.progressKB(), time.Since(p.start))
	return len(b), nil
}

func (p *progressPrinter) Stop() {
	fmt.Fprintf(p.writer, "downloaded %.2fKB, in %v\n", p.progressKB(), time.Since(p.start))
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) {
	return f(p)
}

// Copy copy data from reader to writer
// The copying stopped when context is cancel or writer/reader is closed or EOF
// Copy return context.Canceled error if the context is canceled while copying.
func Copy(ctx context.Context, w io.Writer, r io.Reader) (int64, error) {
	return io.Copy(w, readerFunc(func(p []byte) (int, error) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
			return r.Read(p)
		}
	}))
}
