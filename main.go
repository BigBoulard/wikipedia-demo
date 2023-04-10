package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/freshman-tech/news-demo/logger"
	"github.com/rs/xid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
)

var tpl *template.Template

var HTTPClient = http.Client{
	Timeout: 30 * time.Second,
}

type WikipediaSearchResponse struct {
	BatchComplete string `json:"batchcomplete"`
	Continue      struct {
		Sroffset int    `json:"sroffset"`
		Continue string `json:"continue"`
	} `json:"continue"`
	Query struct {
		SearchInfo struct {
			TotalHits int `json:"totalhits"`
		} `json:"searchinfo"`
		Search []struct {
			Ns        int       `json:"ns"`
			Title     string    `json:"title"`
			PageID    int       `json:"pageid"`
			Size      int       `json:"size"`
			WordCount int       `json:"wordcount"`
			Snippet   string    `json:"snippet"`
			Timestamp time.Time `json:"timestamp"`
		} `json:"search"`
	} `json:"query"`
}

type Search struct {
	Query      string
	TotalPages int
	NextPage   int
	Results    *WikipediaSearchResponse
}

func (s *Search) IsLastPage() bool {
	return s.NextPage >= s.TotalPages
}

func (s *Search) CurrentPage() int {
	if s.NextPage == 1 {
		return s.NextPage
	}

	return s.NextPage - 1
}

func (s *Search) PreviousPage() int {
	return s.CurrentPage() - 1
}

type handlerWithError func(w http.ResponseWriter, r *http.Request) error

func (fn handlerWithError) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := fn(w, r)
	if err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// loggingResponseWriter because there's no way to access the status code of the response through the http.ResponseWriter type
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

// The loggingResponseWriter struct embeds the http.ResponseWriter type
// and adds a statusCode property which defaults to http.StatusOK (200) when newLoggingResponseWriter() is called.
func newLoggingResponseWriter(w http.ResponseWriter) *loggingResponseWriter {
	return &loggingResponseWriter{w, http.StatusOK}
}

// updates the value of the statusCode property and calls the WriteHeader() method of the embedded http.ResponseWriter instance.
func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

func indexHandler(w http.ResponseWriter, r *http.Request) error {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return nil
	}

	buf := &bytes.Buffer{}
	err := tpl.Execute(buf, nil)
	if err != nil {
		return err
	}

	_, err = buf.WriteTo(w)

	return err
}

func searchWikipedia(
	searchQuery string,
	pageSize, resultsOffset int,
) (*WikipediaSearchResponse, error) {
	resp, err := HTTPClient.Get(
		fmt.Sprintf(
			"https://en.wikipedia.org/w/api.php?action=query&list=search&prop=info&inprop=url&utf8=&format=json&origin=*&srlimit=%d&srsearch=%s&sroffset=%d",
			pageSize,
			searchQuery,
			resultsOffset,
		),
	)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respData, _ := httputil.DumpResponse(resp, true)

		return nil, fmt.Errorf(
			"non 200 OK response from Wikipedia API: %s",
			string(respData),
		)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var searchResponse WikipediaSearchResponse

	err = json.Unmarshal(body, &searchResponse)
	if err != nil {
		return nil, err
	}

	return &searchResponse, nil
}

func searchHandler(w http.ResponseWriter, r *http.Request) error {
	u, err := url.Parse(r.URL.String())
	if err != nil {
		return err
	}

	params := u.Query()
	searchQuery := params.Get("q")
	pageNum := params.Get("page")
	if pageNum == "" {
		pageNum = "1"
	}

	// get the logger from the request context
	l := zerolog.Ctx(r.Context())
	// update the logger context to add the "search_query" & "page_num" fields
	l.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Str("search_query", searchQuery).Str("page_num", pageNum)
	})

	// log search query
	l.Info().
		Msgf("incoming search query '%s' on page '%s'", searchQuery, pageNum)

	nextPage, err := strconv.Atoi(pageNum)
	if err != nil {
		return err
	}

	pageSize := 20

	resultsOffset := (nextPage - 1) * pageSize

	searchResponse, err := searchWikipedia(searchQuery, pageSize, resultsOffset)
	if err != nil {
		return err
	}

	// log response from the Wikipedia API
	l.Debug().Interface("wikipedia_search_response", searchResponse).Send()

	totalHits := searchResponse.Query.SearchInfo.TotalHits

	search := &Search{
		Query:      searchQuery,
		Results:    searchResponse,
		TotalPages: int(math.Ceil(float64(totalHits) / float64(pageSize))),
		NextPage:   nextPage + 1,
	}

	buf := &bytes.Buffer{}
	err = tpl.Execute(buf, search)
	if err != nil {
		return err
	}

	_, err = buf.WriteTo(w)
	if err != nil {
		return err
	}

	// log success
	l.Trace().Msgf("search query '%s' succeeded without errors", searchQuery)

	return nil
}

// logger middleware returns an HTTP handler that logs several details about the HTTP request
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		l := logger.Get()

		// generated the correlationID
		correlationID := xid.New().String()
		// add the correlationID to the request context
		ctx := context.WithValue(r.Context(), "correlation_id", correlationID)
		r = r.WithContext(ctx)
		// update the logger context to include the correlationID
		l.UpdateContext(func(c zerolog.Context) zerolog.Context {
			return c.Str("correlation_id", correlationID)
		})
		// set the correlationID in the response headers
		w.Header().Add("X-Correlation-ID", correlationID)

		lrw := newLoggingResponseWriter(w)

		// ensure that the Logger used in the requestLogger() is added to the request context
		// so that it is accessible in all handler functions.
		// l.WithContext associates the Logger instance with the provided context
		// and returns a copy of the context which is immediately used as the argument to r.WithContext(),
		/// which returns a copy of the request with the new context.
		r = r.WithContext(l.WithContext(r.Context()))

		// defer func(){}(): ensure that if the handler panics, the request will still be logged so that you can find out what caused the panic
		defer func() {
			panicVal := recover()
			if panicVal != nil {
				lrw.statusCode = http.StatusInternalServerError // ensure that the status code is updated
				panic(panicVal)                                 // continue panicking
			}

			l.
				Info().
				Str("method", r.Method).
				Str("url", r.URL.RequestURI()).
				Str("user_agent", r.UserAgent()).
				Dur("elapsed_ms", time.Since(start)).
				Int("status_code", lrw.statusCode).
				Msg("incoming request")
		}()

		next.ServeHTTP(lrw, r)
	})
}

func requestLogger2(next http.Handler) http.Handler {
	l := logger.Get()

	h := hlog.NewHandler(l) // injects l into the request context, returns a handler ( r.WithContext(l.WithContext(r.Context())) )

	accessHandler := hlog.AccessHandler( // scalled after each request
		func(r *http.Request, status, size int, duration time.Duration) {
			hlog.FromRequest(r).Info().
				Str("method", r.Method).
				Stringer("url", r.URL).
				Int("status_code", status).
				Int("response_size_bytes", size).
				Dur("elapsed_ms", duration).
				Msg("incoming request")
		},
	)

	userAgentHandler := hlog.UserAgentHandler("http_user_agent") // adds the User Agent to the Logger's

	return h(accessHandler(userAgentHandler(next)))
}

func htmlSafe(str string) template.HTML {
	return template.HTML(str)
}

var err error

func init() {
	l := logger.Get()

	tpl, err = template.New("index.html").Funcs(template.FuncMap{
		"htmlSafe": htmlSafe,
	}).ParseFiles("index.html")
	if err != nil {
		l.Fatal().Err(err).Msg("Unable to initialize HTML templates")
	}
}

func main() {
	l := logger.Get()

	fs := http.FileServer(http.Dir("assets"))

	port := os.Getenv("PORT")
	if port == "" {
		port = "3001"
	}

	mux := http.NewServeMux()
	mux.Handle("/assets/", http.StripPrefix("/assets/", fs))
	mux.Handle("/search", handlerWithError(searchHandler))
	mux.Handle("/", handlerWithError(indexHandler))

	l.Info().
		Str("port", port).
		Msgf("Starting Wikipedia App Server on port '%s'", port)

	l.Fatal().
		Err(http.ListenAndServe(":"+port, requestLogger(mux))).
		Msg("Wikipedia App Server Closed")
}
