package gin

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin/binding"
	"html/template"
	"log"
	"math"
	"net/http"
	"path"
	"sync"
)

const (
	AbortIndex = math.MaxInt8 / 2
	MIMEJSON   = "application/json"
	MIMEHTML   = "text/html"
	MIMEXML    = "application/xml"
	MIMEXML2   = "text/xml"
	MIMEPlain  = "text/plain"
)

// Param is a single URL parameter, consisting of a key and a value.
type Param struct {
	Key   string
	Value string
}

// Params is a Param-slice, as returned by the router.
// The slice is ordered, the first URL parameter is also the first slice value.
// It is therefore save to read values by the index.
type Params []Param

// ByName returns the value of the first Param which key matches the given name.
// If no matching Param is found, an empty string is returned.
func (ps Params) ByName(name string) string {
	for i := range ps {
		if ps[i].Key == name {
			return ps[i].Value
		}
	}
	return ""
}

type (
	HandlerFunc func(*Context)

	H map[string]interface{}

	// Used internally to collect errors that occurred during an http request.
	ErrorMsg struct {
		Err  string      `json:"error"`
		Meta interface{} `json:"meta"`
	}

	ErrorMsgs []ErrorMsg

	// Context is the most important part of gin. It allows us to pass variables between middleware,
	// manage the flow, validate the JSON of a request and render a JSON response for example.
	Context struct {
		Req      *http.Request
		Writer   ResponseWriter
		Keys     map[string]interface{}
		Errors   ErrorMsgs
		Params   Params
		Engine   *Engine
		handlers []HandlerFunc
		index    int8
	}

	// Used internally to configure router, a RouterGroup is associated with a prefix
	// and an array of handlers (middlewares)
	RouterGroup struct {
		Handlers []HandlerFunc
		prefix   string
		parent   *RouterGroup
		engine   *Engine
	}

	// Represents the web framework, it wraps the blazing fast httprouter multiplexer and a list of global middlewares.
	Engine struct {
		*RouterGroup
		HTMLTemplates *template.Template
		cache         sync.Pool
		handlers404   []HandlerFunc

		// Enables automatic redirection if the current route can't be matched but a
		// handler for the path with (without) the trailing slash exists.
		// For example if /foo/ is requested but a route only exists for /foo, the
		// client is redirected to /foo with http status code 301 for GET requests
		// and 307 for all other request methods.
		RedirectTrailingSlash bool

		// If enabled, the router tries to fix the current request path, if no
		// handle is registered for it.
		// First superfluous path elements like ../ or // are removed.
		// Afterwards the router does a case-insensitive lookup of the cleaned path.
		// If a handle can be found for this route, the router makes a  redirection
		// to the corrected path with status code 301 for GET requests and 307 for
		// all other request methods.
		// For example /FOO and /..//Foo could be redirected to /foo.
		// RedirectTrailingSlash is independent of this option.
		RedirectFixedPath bool

		// router
		trees map[string]*node
	}
)

// Allows type H to be used with xml.Marshal
func (h H) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{"", "map"}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	for key, value := range h {
		elem := xml.StartElement{
			xml.Name{"", key},
			[]xml.Attr{},
		}
		if err := e.EncodeElement(value, elem); err != nil {
			return err
		}
	}
	if err := e.EncodeToken(xml.EndElement{start.Name}); err != nil {
		return err
	}
	return nil
}

func (a ErrorMsgs) String() string {
	var buffer bytes.Buffer
	for i, msg := range a {
		text := fmt.Sprintf("Error #%02d: %s \n     Meta: %v\n", (i + 1), msg.Err, msg.Meta)
		buffer.WriteString(text)
	}
	buffer.WriteString("\n")
	return buffer.String()
}

// Returns a new blank Engine instance without any middleware attached.
// The most basic configuration
func New() *Engine {
	engine := &Engine{}
	engine.RouterGroup = &RouterGroup{nil, "/", nil, engine}
	engine.cache.New = func() interface{} {
		return &Context{Engine: engine, Writer: &responseWriter{}}
	}
	return engine
}

// Returns a Engine instance with the Logger and Recovery already attached.
func Default() *Engine {
	engine := New()
	engine.Use(Recovery(), Logger())
	return engine
}

func (engine *Engine) LoadHTMLTemplates(pattern string) {
	engine.HTMLTemplates = template.Must(template.ParseGlob(pattern))
}

// Adds handlers for NotFound. It return a 404 code by default.
func (engine *Engine) NotFound404(handlers ...HandlerFunc) {
	engine.handlers404 = handlers
}

func (engine *Engine) handle404(w http.ResponseWriter, req *http.Request) {
	handlers := engine.combineHandlers(engine.handlers404)
	c := engine.createContext(w, req, nil, handlers)
	c.Writer.setStatus(404)
	c.Next()
	if !c.Writer.Written() {
		c.Data(404, MIMEPlain, []byte("404 page not found"))
	}
	engine.cache.Put(c)
}

// ServeHTTP makes the router implement the http.Handler interface.
func (r *Engine) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if root := r.trees[req.Method]; root != nil {
		path := req.URL.Path

		if handlers, ps, tsr := root.getValue(path); handlers != nil {
			c := r.createContext(w, req, ps, handlers)
			c.Next()
			r.cache.Put(c)
			return
		} else if req.Method != "CONNECT" && path != "/" {
			code := 301 // Permanent redirect, request with GET method
			if req.Method != "GET" {
				// Temporary redirect, request with same method
				// As of Go 1.3, Go does not support status code 308.
				code = 307
			}

			if tsr && r.RedirectTrailingSlash {
				if path[len(path)-1] == '/' {
					req.URL.Path = path[:len(path)-1]
				} else {
					req.URL.Path = path + "/"
				}
				http.Redirect(w, req, req.URL.String(), code)
				return
			}

			// Try to fix the request path
			if r.RedirectFixedPath {
				fixedPath, found := root.findCaseInsensitivePath(
					CleanPath(path),
					r.RedirectTrailingSlash,
				)
				if found {
					req.URL.Path = string(fixedPath)
					http.Redirect(w, req, req.URL.String(), code)
					return
				}
			}
		}
	}
	// Handle 404
	r.handle404(w, req)
}

func (engine *Engine) Run(addr string) {
	if err := http.ListenAndServe(addr, engine); err != nil {
		panic(err)
	}
}

/************************************/
/********** ROUTES GROUPING *********/
/************************************/

func (engine *Engine) createContext(w http.ResponseWriter, req *http.Request, params Params, handlers []HandlerFunc) *Context {
	c := engine.cache.Get().(*Context)
	c.Writer.reset(w)
	c.Req = req
	c.Params = params
	c.handlers = handlers
	c.Keys = nil
	c.index = -1
	return c
}

// Adds middlewares to the group, see example code in github.
func (group *RouterGroup) Use(middlewares ...HandlerFunc) {
	group.Handlers = append(group.Handlers, middlewares...)
}

// Creates a new router group. You should add all the routes that have common middlwares or the same path prefix.
// For example, all the routes that use a common middlware for authorization could be grouped.
func (group *RouterGroup) Group(component string, handlers ...HandlerFunc) *RouterGroup {
	prefix := path.Join(group.prefix, component)
	return &RouterGroup{
		Handlers: group.combineHandlers(handlers),
		parent:   group,
		prefix:   prefix,
		engine:   group.engine,
	}
}

// Handle registers a new request handle and middlewares with the given path and method.
// The last handler should be the real handler, the other ones should be middlewares that can and should be shared among different routes.
// See the example code in github.
//
// For GET, POST, PUT, PATCH and DELETE requests the respective shortcut
// functions can be used.
//
// This function is intended for bulk loading and to allow the usage of less
// frequently used, non-standardized or custom methods (e.g. for internal
// communication with a proxy).
func (group *RouterGroup) Handle(method, p string, handlers []HandlerFunc) {
	p = path.Join(group.prefix, p)
	handlers = group.combineHandlers(handlers)

	if p[0] != '/' {
		panic("path must begin with '/'")
	}

	if group.engine.trees == nil {
		group.engine.trees = make(map[string]*node)
	}

	root := group.engine.trees[method]
	if root == nil {
		root = new(node)
		group.engine.trees[method] = root
	}

	root.addRoute(p, handlers)
}

// POST is a shortcut for router.Handle("POST", path, handle)
func (group *RouterGroup) POST(path string, handlers ...HandlerFunc) {
	group.Handle("POST", path, handlers)
}

// GET is a shortcut for router.Handle("GET", path, handle)
func (group *RouterGroup) GET(path string, handlers ...HandlerFunc) {
	group.Handle("GET", path, handlers)
}

// DELETE is a shortcut for router.Handle("DELETE", path, handle)
func (group *RouterGroup) DELETE(path string, handlers ...HandlerFunc) {
	group.Handle("DELETE", path, handlers)
}

// PATCH is a shortcut for router.Handle("PATCH", path, handle)
func (group *RouterGroup) PATCH(path string, handlers ...HandlerFunc) {
	group.Handle("PATCH", path, handlers)
}

// PUT is a shortcut for router.Handle("PUT", path, handle)
func (group *RouterGroup) PUT(path string, handlers ...HandlerFunc) {
	group.Handle("PUT", path, handlers)
}

// OPTIONS is a shortcut for router.Handle("OPTIONS", path, handle)
func (group *RouterGroup) OPTIONS(path string, handlers ...HandlerFunc) {
	group.Handle("OPTIONS", path, handlers)
}

// HEAD is a shortcut for router.Handle("HEAD", path, handle)
func (group *RouterGroup) HEAD(path string, handlers ...HandlerFunc) {
	group.Handle("HEAD", path, handlers)
}

func (group *RouterGroup) combineHandlers(handlers []HandlerFunc) []HandlerFunc {
	s := len(group.Handlers) + len(handlers)
	h := make([]HandlerFunc, 0, s)
	h = append(h, group.Handlers...)
	h = append(h, handlers...)
	return h
}

/************************************/
/****** FLOW AND ERROR MANAGEMENT****/
/************************************/

func (c *Context) Copy() *Context {
	var cp Context = *c
	cp.index = AbortIndex
	cp.handlers = nil
	return &cp
}

// Next should be used only in the middlewares.
// It executes the pending handlers in the chain inside the calling handler.
// See example in github.
func (c *Context) Next() {
	c.index++
	s := int8(len(c.handlers))
	for ; c.index < s; c.index++ {
		c.handlers[c.index](c)
	}
}

// Forces the system to do not continue calling the pending handlers.
// For example, the first handler checks if the request is authorized. If it's not, context.Abort(401) should be called.
// The rest of pending handlers would never be called for that request.
func (c *Context) Abort(code int) {
	if code >= 0 {
		c.Writer.WriteHeader(code)
	}
	c.index = AbortIndex
}

// Fail is the same as Abort plus an error message.
// Calling `context.Fail(500, err)` is equivalent to:
// ```
// context.Error("Operation aborted", err)
// context.Abort(500)
// ```
func (c *Context) Fail(code int, err error) {
	c.Error(err, "Operation aborted")
	c.Abort(code)
}

// Attaches an error to the current context. The error is pushed to a list of errors.
// It's a good idea to call Error for each error that occurred during the resolution of a request.
// A middleware can be used to collect all the errors and push them to a database together, print a log, or append it in the HTTP response.
func (c *Context) Error(err error, meta interface{}) {
	c.Errors = append(c.Errors, ErrorMsg{
		Err:  err.Error(),
		Meta: meta,
	})
}

func (c *Context) LastError() error {
	s := len(c.Errors)
	if s > 0 {
		return errors.New(c.Errors[s-1].Err)
	} else {
		return nil
	}
}

/************************************/
/******** METADATA MANAGEMENT********/
/************************************/

// Sets a new pair key/value just for the specified context.
// It also lazy initializes the hashmap.
func (c *Context) Set(key string, item interface{}) {
	if c.Keys == nil {
		c.Keys = make(map[string]interface{})
	}
	c.Keys[key] = item
}

// Get returns the value for the given key or an error if the key does not exist.
func (c *Context) Get(key string) (interface{}, error) {
	if c.Keys != nil {
		item, ok := c.Keys[key]
		if ok {
			return item, nil
		}
	}
	return nil, errors.New("Key does not exist.")
}

// MustGet returns the value for the given key or panics if the value doesn't exist.
func (c *Context) MustGet(key string) interface{} {
	value, err := c.Get(key)
	if err != nil || value == nil {
		log.Panicf("Key %s doesn't exist", key)
	}
	return value
}

/************************************/
/******** ENCOGING MANAGEMENT********/
/************************************/

func filterFlags(content string) string {
	for i, a := range content {
		if a == ' ' || a == ';' {
			return content[:i]
		}
	}
	return content
}

// This function checks the Content-Type to select a binding engine automatically,
// Depending the "Content-Type" header different bindings are used:
// "application/json" --> JSON binding
// "application/xml"  --> XML binding
// else --> returns an error
// if Parses the request's body as JSON if Content-Type == "application/json"  using JSON or XML  as a JSON input. It decodes the json payload into the struct specified as a pointer.Like ParseBody() but this method also writes a 400 error if the json is not valid.
func (c *Context) Bind(obj interface{}) bool {
	var b binding.Binding
	ctype := filterFlags(c.Req.Header.Get("Content-Type"))
	switch {
	case c.Req.Method == "GET":
		b = binding.Form
	case ctype == MIMEJSON:
		b = binding.JSON
	case ctype == MIMEXML || ctype == MIMEXML2:
		b = binding.XML
	default:
		c.Fail(400, errors.New("unknown content-type: "+ctype))
		return false
	}
	return c.BindWith(obj, b)
}

func (c *Context) BindWith(obj interface{}, b binding.Binding) bool {
	if err := b.Bind(c.Req, obj); err != nil {
		c.Fail(400, err)
		return false
	}
	return true
}

// Serializes the given struct as JSON into the response body in a fast and efficient way.
// It also sets the Content-Type as "application/json".
func (c *Context) JSON(code int, obj interface{}) {
	c.Writer.Header().Set("Content-Type", MIMEJSON)
	if code >= 0 {
		c.Writer.WriteHeader(code)
	}
	encoder := json.NewEncoder(c.Writer)
	if err := encoder.Encode(obj); err != nil {
		c.Error(err, obj)
		http.Error(c.Writer, err.Error(), 500)
	}
}

// Serializes the given struct as XML into the response body in a fast and efficient way.
// It also sets the Content-Type as "application/xml".
func (c *Context) XML(code int, obj interface{}) {
	c.Writer.Header().Set("Content-Type", MIMEXML)
	if code >= 0 {
		c.Writer.WriteHeader(code)
	}
	encoder := xml.NewEncoder(c.Writer)
	if err := encoder.Encode(obj); err != nil {
		c.Error(err, obj)
		http.Error(c.Writer, err.Error(), 500)
	}
}

// Renders the HTTP template specified by its file name.
// It also updates the HTTP code and sets the Content-Type as "text/html".
// See http://golang.org/doc/articles/wiki/
func (c *Context) HTML(code int, name string, data interface{}) {
	c.Writer.Header().Set("Content-Type", MIMEHTML)
	if code >= 0 {
		c.Writer.WriteHeader(code)
	}
	if err := c.Engine.HTMLTemplates.ExecuteTemplate(c.Writer, name, data); err != nil {
		c.Error(err, map[string]interface{}{
			"name": name,
			"data": data,
		})
		http.Error(c.Writer, err.Error(), 500)
	}
}

// Writes the given string into the response body and sets the Content-Type to "text/plain".
func (c *Context) String(code int, format string, values ...interface{}) {
	c.Writer.Header().Set("Content-Type", MIMEPlain)
	if code >= 0 {
		c.Writer.WriteHeader(code)
	}
	c.Writer.Write([]byte(fmt.Sprintf(format, values...)))
}

// Writes some data into the body stream and updates the HTTP code.
func (c *Context) Data(code int, contentType string, data []byte) {
	if len(contentType) > 0 {
		c.Writer.Header().Set("Content-Type", contentType)
	}
	if code >= 0 {
		c.Writer.WriteHeader(code)
	}
	c.Writer.Write(data)
}
