package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/datadog-go/v5/statsd"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"
	"github.com/xvello/letsblockit/src/filters"
	"github.com/xvello/letsblockit/src/pages"
	"github.com/xvello/letsblockit/src/store"
)

var ErrDryRunFinished = errors.New("dry run finished")

type Options struct {
	Address      string `default:"127.0.0.1:8765" help:"address to listen to"`
	Debug        bool   `help:"log with debug level"`
	DryRun       bool   `arg:"--dry-run" help:"instantiate all components and exit"`
	Migrations   bool   `help:"run gorm schema migrations on startup"`
	OryProject   string `help:"oxy cloud project to check credentials against"`
	Reload       bool   `help:"reload frontend when the backend restarts"`
	Statsd       string `help:"address to send statsd metrics to"`
	DatabaseName string `default:"letsblockit" help:"psql database name to use"`
	DatabaseHost string `default:"/var/run/postgresql" help:"psql host to connect to"`
	silent       bool
}

var navigationLinks = []struct {
	Name   string
	Target string
}{{
	Name:   "Filter list",
	Target: "filters",
}, {
	Name:   "About",
	Target: "about",
}}

type Server struct {
	assets  *wrappedAssets
	echo    *echo.Echo
	options *Options
	store   DataStore
	filters FilterRepository
	pages   PageRenderer
}

func NewServer(options *Options) *Server {
	return &Server{
		options: options,
		echo:    echo.New(),
	}
}

func (s *Server) Start() error {
	concurrentRunOrPanic([]func([]error){
		func(_ []error) { s.assets = loadAssets() },
		func(errs []error) { s.pages, errs[0] = pages.LoadPages() },
		func(errs []error) { s.filters, errs[0] = filters.LoadFilters() },
		func(errs []error) {
			var storeOpts []store.Option
			if s.options.Migrations {
				storeOpts = append(storeOpts, store.Migrate)
			}
			s.store, errs[0] = store.NewStore(s.options.DatabaseHost, s.options.DatabaseName, storeOpts...)
		},
	})

	if s.options.Statsd != "" {
		dsd, err := statsd.New(s.options.Statsd)
		if err != nil {
			return err
		}
		s.echo.Use(buildDogstatsMiddleware(dsd))
	}

	if s.options.OryProject != "" {
		s.echo.Use(buildOryMiddleware(s.options.OryProject, s.echo.Logger))
	}

	if s.options.Debug {
		s.echo.Logger.SetLevel(log.DEBUG)
	}

	s.pages.RegisterHelpers(buildHelpers(s.echo, s.assets.hash))
	s.setupRouter()
	if s.options.DryRun {
		return ErrDryRunFinished
	}
	return s.echo.Start(s.options.Address)
}

func (s *Server) setupRouter() {
	if !s.options.silent {
		s.echo.Use(middleware.Logger())
	}
	s.echo.Pre(middleware.RemoveTrailingSlash())
	s.echo.Pre(middleware.Rewrite(map[string]string{
		"/favicon.ico": "/assets/images/favicon.ico",
		"/":            "/filters",
	}))

	if s.options.Reload {
		s.echo.GET("/should-reload", func(c echo.Context) error {
			// Set the headers related to event streaming.
			c.Response().Header().Set("Content-Type", "text/event-stream")
			c.Response().Header().Set("Cache-Control", "no-cache")
			c.Response().Header().Set("Connection", "keep-alive")
			c.Response().Header().Set("Transfer-Encoding", "chunked")
			if _, err := fmt.Fprintln(c.Response(), "retry:1000"); err != nil {
				return nil
			}
			c.Response().Flush()

			// Block indefinitely to keep the SSE open
			<-c.Request().Context().Done()
			return nil
		})
	}

	s.echo.GET("/assets/*", s.assets.serve)

	s.addStatic("/about", "about", "About: Let’s block it!")

	s.echo.GET("/filters", s.listFilters).Name = "list-filters"
	s.echo.GET("/filters/tag/:tag", s.listFilters).Name = "filters-for-tag"

	s.echo.GET("/filters/:name", s.viewFilter).Name = "view-filter"
	s.echo.POST("/filters/:name", s.viewFilter)
	s.echo.POST("/filters/:name/render", s.viewFilterRender).Name = "view-filter-render"

	s.echo.GET("/list/:token", s.renderList).Name = "render-filterlist"

	s.echo.GET("/user/login", s.userLogin).Name = "user-login"
	s.echo.GET("/user/logout", s.userLogout).Name = "user-logout"
	s.echo.GET("/user/account", s.userAccount).Name = "user-account"
}

func (s *Server) addStatic(url, page, title string) {
	s.echo.GET(url, func(c echo.Context) error {
		return s.pages.Render(c, page, s.buildPageContext(c, title))
	}).Name = page
}

// redirect the user to another page, either via htmx client-side redirect (form submissions)
// or http 302 redirect (direct access, js disabled)
func (s *Server) redirect(c echo.Context, name string, params ...interface{}) error {
	target := s.echo.Reverse(name, params...)
	if c.Request().Header.Get("HX-Request") == "true" {
		c.Response().Header().Set("HX-Redirect", target)
		return nil
	}
	return c.Redirect(http.StatusFound, target)
}

func (s *Server) buildPageContext(c echo.Context, title string) *pages.Context {
	var section string
	for _, s := range strings.Split(c.Path(), "/") {
		if s != "" {
			section = s
			break
		}
	}
	context := &pages.Context{
		CurrentSection:  section,
		NavigationLinks: navigationLinks,
		Title:           title,
	}
	if u := getUser(c); u != nil {
		context.UserID = u.Id()
		context.UserVerified = u.IsVerified()
	}
	if s.options.Reload {
		context.Scripts = []string{"reload.js"}
	}
	return context
}

func concurrentRunOrPanic(tasks []func([]error)) {
	var wg sync.WaitGroup
	timings := make([]time.Duration, len(tasks))
	for i, f := range tasks {
		wg.Add(1)
		f := f
		i := i
		go func(wg *sync.WaitGroup) {
			start := time.Now()
			defer wg.Done()
			errs := []error{nil}
			f(errs)
			if errs[0] != nil {
				panic(errs[0])
			}
			timings[i] = time.Since(start)
		}(&wg)
	}
	wg.Wait()

	var output strings.Builder
	output.WriteString("Startup tasks finished |")
	for i, d := range timings {
		output.WriteString(fmt.Sprintf(" %d: %s |", i, d))
	}
	fmt.Println(output.String())
}

func buildDogstatsMiddleware(dsd statsd.ClientInterface) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			if err := next(c); err != nil {
				c.Error(err)
			}
			loggedTag := fmt.Sprintf("logged:%t", c.Get(userContextKey) != nil)
			duration := time.Since(start)
			_ = dsd.Distribution("letsblockit.request_duration", float64(duration.Nanoseconds()), []string{loggedTag}, 1)
			_ = dsd.Incr("letsblockit.request_count", []string{loggedTag, fmt.Sprintf("status:%d", c.Response().Status)}, 1)
			return nil
		}
	}
}
