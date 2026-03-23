package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTargetOrigin = "https://example.com"
	contextTTL          = 10 * time.Minute
	responseCacheTTL    = 10 * time.Minute
	responseCacheMaxObj = 4 * 1024 * 1024
	responseCacheMaxMem = 64 * 1024 * 1024
	responseCacheMaxNum = 256
	canonicalProxyPath  = "/go"
)

var (
	errRouteNotFound = errors.New("route not found")

	textContentTypes = []string{
		"text/html",
	}

	escapedAbsoluteURLPattern = regexp.MustCompile(`(?i)(https?):\\\/\\\/([A-Za-z0-9.-]+(?::\d+)?)`)
	absoluteURLPattern        = regexp.MustCompile(`(?i)(https?):\/\/([A-Za-z0-9.-]+(?::\d+)?)`)
	escapedProtoRelativeURL   = regexp.MustCompile(`(?i)(^|[^:])(\\\/\\\/)([A-Za-z0-9.-]+(?::\d+)?)`)
	protoRelativeURL          = regexp.MustCompile(`(?i)(^|[^:])(\/\/)([A-Za-z0-9.-]+(?::\d+)?)`)
	rootRelativeAttrPattern   = regexp.MustCompile(`(?i)(\b(?:href|src|action|poster|data-href)\s*=\s*["'])/([^"']*)`)
	baseTagPattern            = regexp.MustCompile(`(?i)<base\s`)
	headTagPattern            = regexp.MustCompile(`(?i)<head([^>]*)>`)
	serverURLPattern          = regexp.MustCompile(`serverUrl:\s*['"][^'"]*['"]`)
	setCookieDomainPattern    = regexp.MustCompile(`(?i);\s*Domain=[^;]+`)
)

type config struct {
	DefaultTargetOrigin string
	DefaultTargetBase   *url.URL
	AllowedHostSuffixes []string
	AllowedTargetText   string
	LocalPort           int
	UpstreamProxyURL    string
	UpstreamProxyOn     bool
	AccessLogOn         bool
	ConfigFilePath      string
}

type fileConfig struct {
	TargetURL           string   `json:"target_url"`
	AllowedHostSuffixes []string `json:"allowed_host_suffixes"`
	Port                *int     `json:"port"`
	UpstreamProxyURL    string   `json:"upstream_proxy_url"`
	UpstreamProxyOn     *bool    `json:"upstream_proxy_on"`
	AccessLog           *bool    `json:"access_log"`
}

type routeContext struct {
	RouteMode        string
	Target           *url.URL
	TargetURL        *url.URL
	Prefix           string
	PublicBase       string
	PathKey          string
	RedirectLocation string
}

type contextualTargetEntry struct {
	TargetOrigin string
	ExpiresAt    time.Time
}

type responseCacheEntry struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	ExpiresAt  time.Time
	LastAccess time.Time
	Size       int
}

type replacementEntry struct {
	Placeholder string
	Value       string
}

type server struct {
	cfg                config
	log                *log.Logger
	mu                 sync.Mutex
	contextCache       map[string]contextualTargetEntry
	responseCache      map[string]responseCacheEntry
	responseCacheBytes int
	http               *http.Client
}

type socks5Dialer struct {
	proxyURL *url.URL
	base     net.Dialer
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	client, err := buildHTTPClient(cfg)
	if err != nil {
		log.Fatal(err)
	}

	srv := &server{
		cfg:           cfg,
		log:           log.New(os.Stdout, "", 0),
		contextCache:  make(map[string]contextualTargetEntry),
		responseCache: make(map[string]responseCacheEntry),
		http:          client,
	}

	httpServer := &http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", cfg.LocalPort),
		Handler:           http.HandlerFunc(srv.handle),
		ReadHeaderTimeout: 15 * time.Second,
	}

	srv.log.Println("========================================")
	srv.log.Println("Domain-group proxy server is running.")
	srv.log.Printf("Listen: http://0.0.0.0:%d\n", cfg.LocalPort)
	srv.log.Printf("Default target: %s\n", cfg.DefaultTargetBase.String())
	srv.log.Printf("Allowed hosts: %s\n", cfg.AllowedTargetText)
	if cfg.ConfigFilePath != "" {
		srv.log.Printf("Config file: %s\n", cfg.ConfigFilePath)
	} else {
		srv.log.Println("Config file: not found, using built-in defaults/env")
	}
	if cfg.UpstreamProxyOn {
		srv.log.Printf("Upstream SOCKS5: enabled (%s)\n", maskProxyURL(cfg.UpstreamProxyURL))
	} else {
		srv.log.Println("Upstream SOCKS5: disabled")
	}
	if cfg.AccessLogOn {
		srv.log.Println("Access log: enabled")
	} else {
		srv.log.Println("Access log: disabled")
	}
	srv.log.Printf("Main entry: http://your-ip:%d/go\n", cfg.LocalPort)
	srv.log.Println("========================================")

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func loadConfig() (config, error) {
	fileCfg, configFilePath, err := loadFileConfig()
	if err != nil {
		return config{}, err
	}

	targetOrigin := strings.TrimSpace(fileCfg.TargetURL)
	if targetOrigin == "" {
		targetOrigin = defaultTargetOrigin
	}
	if envTargetOrigin, ok := lookupNonEmptyEnv("TARGET_URL"); ok {
		targetOrigin = envTargetOrigin
	}

	targetURL, err := url.Parse(targetOrigin)
	if err != nil {
		return config{}, fmt.Errorf("invalid TARGET_URL: %w", err)
	}
	if !targetURL.IsAbs() || targetURL.Host == "" {
		return config{}, fmt.Errorf("invalid TARGET_URL: %s", targetOrigin)
	}
	defaultTargetBase := originURL(targetURL)

	allowedSuffixes := normalizeHostSuffixes(fileCfg.AllowedHostSuffixes)
	if len(allowedSuffixes) == 0 {
		allowedSuffixes = []string{strings.ToLower(targetURL.Hostname())}
	}
	if allowedSuffixesRaw, ok := lookupNonEmptyEnv("ALLOWED_HOST_SUFFIXES"); ok {
		allowedSuffixes = splitLowerCSV(allowedSuffixesRaw)
	}
	if len(allowedSuffixes) == 0 {
		return config{}, fmt.Errorf("ALLOWED_HOST_SUFFIXES is empty")
	}

	port := 16800
	if fileCfg.Port != nil {
		port = *fileCfg.Port
	}
	if envPortText, ok := lookupNonEmptyEnv("PORT"); ok {
		port, err = strconv.Atoi(envPortText)
		if err != nil || port <= 0 {
			return config{}, fmt.Errorf("invalid PORT: %s", envPortText)
		}
	} else if port <= 0 {
		return config{}, fmt.Errorf("invalid port in config file: %d", port)
	}

	upstreamProxyURL := strings.TrimSpace(fileCfg.UpstreamProxyURL)
	if envUpstreamProxyURL, ok := lookupNonEmptyEnv("UPSTREAM_PROXY_URL"); ok {
		upstreamProxyURL = envUpstreamProxyURL
	}

	upstreamProxyOn := false
	if fileCfg.UpstreamProxyOn != nil {
		upstreamProxyOn = *fileCfg.UpstreamProxyOn
	}
	if envUpstreamProxyOn, ok, err := lookupEnvBool("UPSTREAM_PROXY_ON"); err != nil {
		return config{}, err
	} else if ok {
		upstreamProxyOn = envUpstreamProxyOn
	}

	accessLogOn := false
	if fileCfg.AccessLog != nil {
		accessLogOn = *fileCfg.AccessLog
	}
	if envAccessLogOn, ok, err := lookupEnvBool("ACCESS_LOG"); err != nil {
		return config{}, err
	} else if ok {
		accessLogOn = envAccessLogOn
	}

	return config{
		DefaultTargetOrigin: targetOrigin,
		DefaultTargetBase:   defaultTargetBase,
		AllowedHostSuffixes: allowedSuffixes,
		AllowedTargetText:   strings.Join(toAllowedTargetText(allowedSuffixes), ", "),
		LocalPort:           port,
		UpstreamProxyURL:    upstreamProxyURL,
		UpstreamProxyOn:     upstreamProxyOn,
		AccessLogOn:         accessLogOn,
		ConfigFilePath:      configFilePath,
	}, nil
}

func buildHTTPClient(cfg config) (*http.Client, error) {
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	if cfg.UpstreamProxyOn {
		if strings.TrimSpace(cfg.UpstreamProxyURL) == "" {
			return nil, fmt.Errorf("upstream SOCKS5 is enabled but URL is empty")
		}

		proxyURL, err := url.Parse(cfg.UpstreamProxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid SOCKS5 proxy URL: %w", err)
		}

		dialer := &socks5Dialer{proxyURL: proxyURL}

		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("unsupported network for SOCKS5: %s", network)
	}

	conn, err := d.base.DialContext(ctx, "tcp", d.proxyURL.Host)
	if err != nil {
		return nil, err
	}

	if err := d.handshake(conn, addr); err != nil {
		conn.Close()
		return nil, err
	}

	return conn, nil
}

func (d *socks5Dialer) handshake(conn net.Conn, targetAddr string) error {
	methods := []byte{0x00}
	username := ""
	password := ""

	if d.proxyURL.User != nil {
		username = d.proxyURL.User.Username()
		password, _ = d.proxyURL.User.Password()
		if username != "" || password != "" {
			methods = []byte{0x00, 0x02}
		}
	}

	greeting := []byte{0x05, byte(len(methods))}
	greeting = append(greeting, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("SOCKS5 greeting failed: %w", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("SOCKS5 greeting reply failed: %w", err)
	}
	if reply[0] != 0x05 {
		return fmt.Errorf("SOCKS5 invalid greeting version: %d", reply[0])
	}
	if reply[1] == 0xFF {
		return fmt.Errorf("SOCKS5 proxy rejected authentication methods")
	}
	if reply[1] == 0x02 {
		if len(username) > 255 || len(password) > 255 {
			return fmt.Errorf("SOCKS5 username/password is too long")
		}

		authPacket := []byte{0x01, byte(len(username))}
		authPacket = append(authPacket, []byte(username)...)
		authPacket = append(authPacket, byte(len(password)))
		authPacket = append(authPacket, []byte(password)...)
		if _, err := conn.Write(authPacket); err != nil {
			return fmt.Errorf("SOCKS5 auth request failed: %w", err)
		}

		authReply := make([]byte, 2)
		if _, err := io.ReadFull(conn, authReply); err != nil {
			return fmt.Errorf("SOCKS5 auth reply failed: %w", err)
		}
		if authReply[1] != 0x00 {
			return fmt.Errorf("SOCKS5 auth failed with code %d", authReply[1])
		}
	}

	host, portText, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return fmt.Errorf("invalid target address %q: %w", targetAddr, err)
	}

	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("invalid target port %q", portText)
	}

	request := []byte{0x05, 0x01, 0x00}
	ip := net.ParseIP(host)
	switch {
	case ip != nil && ip.To4() != nil:
		request = append(request, 0x01)
		request = append(request, ip.To4()...)
	case ip != nil && ip.To16() != nil:
		request = append(request, 0x04)
		request = append(request, ip.To16()...)
	default:
		if len(host) > 255 {
			return fmt.Errorf("SOCKS5 host name is too long")
		}
		request = append(request, 0x03, byte(len(host)))
		request = append(request, []byte(host)...)
	}

	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(port))
	request = append(request, portBytes...)

	if _, err := conn.Write(request); err != nil {
		return fmt.Errorf("SOCKS5 connect request failed: %w", err)
	}

	responseHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, responseHeader); err != nil {
		return fmt.Errorf("SOCKS5 connect reply failed: %w", err)
	}
	if responseHeader[1] != 0x00 {
		return fmt.Errorf("SOCKS5 connect failed with code %d", responseHeader[1])
	}

	var addrLen int
	switch responseHeader[3] {
	case 0x01:
		addrLen = 4
	case 0x04:
		addrLen = 16
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return fmt.Errorf("SOCKS5 domain length read failed: %w", err)
		}
		addrLen = int(lenBuf[0])
	default:
		return fmt.Errorf("SOCKS5 invalid address type %d", responseHeader[3])
	}

	if addrLen > 0 {
		discard := make([]byte, addrLen+2)
		if _, err := io.ReadFull(conn, discard); err != nil {
			return fmt.Errorf("SOCKS5 address read failed: %w", err)
		}
	}

	return nil
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	proxyOrigin := getProxyOrigin(r)
	rawURL := r.URL.RequestURI()

	route, err := s.resolveRoute(rawURL, proxyOrigin, r)
	if err != nil {
		if errors.Is(err, errRouteNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	if route.RedirectLocation != "" {
		w.Header().Set("Location", route.RedirectLocation)
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}

	cacheKey := ""
	if s.isResponseCacheCandidate(r, route) {
		cacheKey = s.buildResponseCacheKey(route)
		if cached, ok := s.getCachedResponse(cacheKey); ok {
			copyHeader(w.Header(), cached.Header)
			w.Header().Set("X-Proxy-Cache", "HIT")
			w.WriteHeader(cached.StatusCode)
			if _, err := w.Write(cached.Body); err != nil {
				s.log.Printf("[%s] cached response write error: %v\n", time.Now().UTC().Format(time.RFC3339), err)
			}
			return
		}
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, route.TargetURL.String(), r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid upstream request: %v", err), http.StatusBadGateway)
		return
	}

	upstreamReq.Header = s.buildProxyHeaders(r, proxyOrigin, route)
	upstreamReq.Host = route.Target.Host

	if s.cfg.AccessLogOn {
		s.log.Printf("[%s] %s %s -> %s\n", time.Now().UTC().Format(time.RFC3339), r.Method, rawURL, route.TargetURL.String())
	}

	upstreamRes, err := s.http.Do(upstreamReq)
	if err != nil {
		s.log.Printf("[%s] proxy request error: %v\n", time.Now().UTC().Format(time.RFC3339), err)
		http.Error(w, fmt.Sprintf("Proxy request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer upstreamRes.Body.Close()

	contentType := strings.ToLower(upstreamRes.Header.Get("Content-Type"))
	rewriteBody := shouldRewriteBody(contentType, route)
	responseHeaders := s.buildResponseHeaders(upstreamRes, rewriteBody, route)

	if !rewriteBody {
		if cacheKey != "" && s.shouldStoreResponseCache(upstreamRes, route) {
			bodyBytes, err := io.ReadAll(upstreamRes.Body)
			if err != nil {
				s.log.Printf("[%s] upstream body read error: %v\n", time.Now().UTC().Format(time.RFC3339), err)
				http.Error(w, fmt.Sprintf("Upstream response error: %v", err), http.StatusBadGateway)
				return
			}

			copyHeader(w.Header(), responseHeaders)
			w.Header().Set("X-Proxy-Cache", "MISS")
			w.WriteHeader(upstreamRes.StatusCode)
			if _, err := w.Write(bodyBytes); err != nil {
				s.log.Printf("[%s] downstream write error: %v\n", time.Now().UTC().Format(time.RFC3339), err)
				return
			}

			s.storeCachedResponse(cacheKey, upstreamRes.StatusCode, responseHeaders, bodyBytes)
			return
		}

		copyHeader(w.Header(), responseHeaders)
		if cacheKey != "" {
			w.Header().Set("X-Proxy-Cache", "BYPASS")
		}
		w.WriteHeader(upstreamRes.StatusCode)
		if _, err := io.Copy(w, upstreamRes.Body); err != nil {
			s.log.Printf("[%s] upstream stream error: %v\n", time.Now().UTC().Format(time.RFC3339), err)
		}
		return
	}

	bodyBytes, err := io.ReadAll(upstreamRes.Body)
	if err != nil {
		s.log.Printf("[%s] upstream body read error: %v\n", time.Now().UTC().Format(time.RFC3339), err)
		http.Error(w, fmt.Sprintf("Upstream response error: %v", err), http.StatusBadGateway)
		return
	}

	rewrittenBody := s.rewriteTextBody(string(bodyBytes), proxyOrigin, route)
	if strings.Contains(contentType, "text/html") {
		rewrittenBody = rewriteHTMLRootRelativeAttrs(rewrittenBody, route)
		rewrittenBody = rewriteEnvServerURL(rewrittenBody, route.PublicBase)
	}
	if shouldRewriteJSRoute(route) {
		rewrittenBody = rewriteOrderPaymentBundle(rewrittenBody)
	}
	if route.RouteMode == "prefixed" && strings.Contains(contentType, "text/html") && !baseTagPattern.MatchString(rewrittenBody) {
		documentBase := getDocumentBaseHref(r, proxyOrigin)
		if headIndex := headTagPattern.FindStringIndex(rewrittenBody); headIndex != nil {
			match := rewrittenBody[headIndex[0]:headIndex[1]]
			rewrittenBody = rewrittenBody[:headIndex[0]] + match + `<base href="` + documentBase + `">` + rewrittenBody[headIndex[1]:]
		}
	}

	responseHeaders.Set("Content-Length", strconv.Itoa(len([]byte(rewrittenBody))))
	copyHeader(w.Header(), responseHeaders)
	if cacheKey != "" && s.shouldStoreResponseCache(upstreamRes, route) {
		w.Header().Set("X-Proxy-Cache", "MISS")
		w.WriteHeader(upstreamRes.StatusCode)
		if _, err := io.WriteString(w, rewrittenBody); err != nil {
			s.log.Printf("[%s] downstream write error: %v\n", time.Now().UTC().Format(time.RFC3339), err)
			return
		}
		s.storeCachedResponse(cacheKey, upstreamRes.StatusCode, responseHeaders, []byte(rewrittenBody))
		return
	}
	if cacheKey != "" {
		w.Header().Set("X-Proxy-Cache", "BYPASS")
	}
	w.WriteHeader(upstreamRes.StatusCode)
	if _, err := io.WriteString(w, rewrittenBody); err != nil {
		s.log.Printf("[%s] downstream write error: %v\n", time.Now().UTC().Format(time.RFC3339), err)
	}
}

func (s *server) resolveRoute(rawURL, proxyOrigin string, r *http.Request) (*routeContext, error) {
	if isMainEntryRequest(rawURL) {
		return s.resolveMainEntryRoute(rawURL, proxyOrigin)
	}

	prefixedRoute, err := s.parsePrefixedRoute(rawURL)
	if err != nil {
		return nil, err
	}
	if prefixedRoute != nil {
		if prefixedRoute.RedirectLocation == "" {
			prefixedRoute.PublicBase = proxyOrigin + prefixedRoute.Prefix
			prefixedRoute.PathKey = buildPathKey(prefixedRoute.TargetURL.Path, prefixedRoute.TargetURL.RawQuery)
			if prefixedRoute.PathKey != "/" {
				s.rememberContextualTarget(prefixedRoute.PathKey, prefixedRoute.Target)
			}
		}
		return prefixedRoute, nil
	}

	if strings.HasPrefix(rawURL, canonicalProxyPath) {
		return nil, errRouteNotFound
	}

	normalizedPath := rawURL
	if normalizedPath == "" || normalizedPath == "/" {
		normalizedPath = "/"
	} else if !strings.HasPrefix(normalizedPath, "/") {
		normalizedPath = "/" + normalizedPath
	}

	pathURL, err := url.Parse(normalizedPath)
	if err != nil {
		return nil, fmt.Errorf("invalid route path: %w", err)
	}

	pathKey := buildPathKey(pathURL.Path, pathURL.RawQuery)
	targetBase := s.getTargetFromReferer(r.Referer(), proxyOrigin)
	if targetBase == nil && pathKey != "/" {
		targetBase = s.getCachedContextualTarget(pathKey)
	}
	if targetBase == nil {
		return nil, errRouteNotFound
	}

	targetURL := targetBase.ResolveReference(pathURL)
	pathKey = buildPathKey(targetURL.Path, targetURL.RawQuery)
	s.rememberContextualTarget(pathKey, targetBase)

	return &routeContext{
		RouteMode:  "root",
		Target:     targetBase,
		TargetURL:  targetURL,
		Prefix:     buildTargetPrefix(targetBase),
		PublicBase: proxyOrigin,
		PathKey:    pathKey,
	}, nil
}

func isMainEntryRequest(rawURL string) bool {
	return rawURL == canonicalProxyPath ||
		rawURL == canonicalProxyPath+"/" ||
		strings.HasPrefix(rawURL, canonicalProxyPath+"?") ||
		strings.HasPrefix(rawURL, canonicalProxyPath+"/?")
}

func (s *server) resolveMainEntryRoute(rawURL, proxyOrigin string) (*routeContext, error) {
	pathURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid route path: %w", err)
	}

	if pathURL.Path != canonicalProxyPath && pathURL.Path != canonicalProxyPath+"/" {
		return nil, errRouteNotFound
	}

	targetBase := cloneURL(s.cfg.DefaultTargetBase)
	targetURL := cloneURL(targetBase)
	targetURL.Path = "/"
	targetURL.RawPath = ""
	targetURL.RawQuery = pathURL.RawQuery

	return &routeContext{
		RouteMode:  "entry",
		Target:     targetBase,
		TargetURL:  targetURL,
		Prefix:     canonicalProxyPath,
		PublicBase: proxyOrigin,
		PathKey:    canonicalProxyPath,
	}, nil
}

func (s *server) parsePrefixedRoute(rawURL string) (*routeContext, error) {
	if route, handled, err := s.parseCanonicalPrefixedRoute(rawURL); handled || err != nil {
		return route, err
	}
	if route, handled, err := s.parseLegacyPrefixedRoute(rawURL); handled || err != nil {
		return route, err
	}
	return nil, nil
}

func (s *server) parseCanonicalPrefixedRoute(rawURL string) (*routeContext, bool, error) {
	prefixRoot := canonicalProxyPath + "/"
	if !strings.HasPrefix(rawURL, prefixRoot) {
		return nil, false, nil
	}

	remainder := strings.TrimPrefix(rawURL, prefixRoot)
	schemeEnd := strings.IndexByte(remainder, '/')
	if schemeEnd <= 0 {
		return nil, false, nil
	}

	scheme := remainder[:schemeEnd]
	if scheme != "http" && scheme != "https" {
		return nil, false, nil
	}

	hostAndPath := remainder[schemeEnd+1:]
	if hostAndPath == "" {
		return nil, true, fmt.Errorf("Invalid target URL.")
	}

	hostEnd := len(hostAndPath)
	if idx := strings.IndexAny(hostAndPath, "/?"); idx >= 0 {
		hostEnd = idx
	}

	host := hostAndPath[:hostEnd]
	if host == "" {
		return nil, true, fmt.Errorf("Invalid target URL.")
	}

	requestURI := hostAndPath[hostEnd:]
	if requestURI == "" {
		requestURI = "/"
	} else if strings.HasPrefix(requestURI, "?") {
		requestURI = "/" + requestURI
	}

	targetURL, err := url.Parse(fmt.Sprintf("%s://%s%s", scheme, host, requestURI))
	if err != nil || !targetURL.IsAbs() || targetURL.Host == "" {
		return nil, true, fmt.Errorf("Invalid target URL.")
	}

	targetBase, err := s.createTargetBase(targetURL)
	if err != nil {
		return nil, true, err
	}

	prefix := buildTargetPrefix(targetBase)
	if rawURL == prefix {
		return &routeContext{RedirectLocation: prefix + "/"}, true, nil
	}
	if strings.HasPrefix(rawURL, prefix+"?") {
		return &routeContext{RedirectLocation: prefix + "/" + rawURL[len(prefix):]}, true, nil
	}

	return &routeContext{
		RouteMode: "prefixed",
		Target:    targetBase,
		TargetURL: targetURL,
		Prefix:    prefix,
	}, true, nil
}

func (s *server) parseLegacyPrefixedRoute(rawURL string) (*routeContext, bool, error) {
	if !strings.HasPrefix(rawURL, "/http://") && !strings.HasPrefix(rawURL, "/https://") {
		return nil, false, nil
	}

	targetURL, err := url.Parse(rawURL[1:])
	if err != nil || !targetURL.IsAbs() || targetURL.Host == "" {
		return nil, true, fmt.Errorf("Invalid target URL.")
	}

	targetBase, err := s.createTargetBase(targetURL)
	if err != nil {
		return nil, true, err
	}

	requestURI := targetURL.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}

	return &routeContext{
		RedirectLocation: buildTargetPrefix(targetBase) + requestURI,
	}, true, nil
}

func (s *server) getTargetFromReferer(referer, proxyOrigin string) *url.URL {
	refererURL := getSameProxyURL(referer, proxyOrigin)
	if refererURL == nil {
		return nil
	}
	if isMainEntryRequest(refererURL.RequestURI()) {
		return cloneURL(s.cfg.DefaultTargetBase)
	}

	refererPathKey := buildPathKey(refererURL.Path, refererURL.RawQuery)
	prefixedRoute, err := s.parsePrefixedRoute(refererPathKey)
	if err == nil && prefixedRoute != nil && prefixedRoute.RedirectLocation == "" {
		return prefixedRoute.Target
	}

	return s.getCachedContextualTarget(refererPathKey)
}

func (s *server) createTargetBase(targetURL *url.URL) (*url.URL, error) {
	if !s.isAllowedTargetURL(targetURL) {
		return nil, fmt.Errorf("Only %s are allowed.", s.cfg.AllowedTargetText)
	}
	return originURL(targetURL), nil
}

func (s *server) isAllowedTargetURL(targetURL *url.URL) bool {
	if targetURL == nil {
		return false
	}
	if targetURL.Scheme != "http" && targetURL.Scheme != "https" {
		return false
	}
	return s.isAllowedHostname(targetURL.Hostname())
}

func (s *server) isAllowedHostname(hostname string) bool {
	host := strings.ToLower(hostname)
	for _, suffix := range s.cfg.AllowedHostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func (s *server) buildProxyHeaders(r *http.Request, proxyOrigin string, route *routeContext) http.Header {
	headers := cloneHeader(r.Header)
	headers.Del("Connection")
	headers.Del("Proxy-Connection")
	headers.Del("Keep-Alive")
	headers.Del("Transfer-Encoding")
	headers.Del("Upgrade")
	if shouldDropSensitiveRequestHeaders(route) {
		headers.Del("Cookie")
		headers.Del("Authorization")
	}

	headers.Set("Host", route.Target.Host)
	forwardedHost := r.Host
	forwardedProto := getProxyProtocol(r)
	forwardedPort := getProxyPort(r)
	originalHost := r.Host
	originalURI := r.URL.RequestURI()
	originalURL := proxyOrigin + r.URL.RequestURI()
	if shouldUseUpstreamOriginHeaders(route) {
		forwardedHost = route.Target.Host
		forwardedProto = route.Target.Scheme
		forwardedPort = targetPort(route.Target)
		originalHost = route.Target.Host
		originalURI = route.TargetURL.RequestURI()
		originalURL = route.TargetURL.String()
	}
	headers.Set("X-Forwarded-Host", forwardedHost)
	headers.Set("X-Forwarded-Proto", forwardedProto)
	headers.Set("X-Forwarded-Port", forwardedPort)
	headers.Set("X-Forwarded-For", appendForwardedFor(r.Header.Get("X-Forwarded-For"), remoteIP(r.RemoteAddr)))
	headers.Set("X-Original-Host", originalHost)
	headers.Set("X-Original-URI", originalURI)
	headers.Set("X-Original-URL", originalURL)
	if route.RouteMode == "prefixed" {
		headers.Set("X-Forwarded-Prefix", route.Prefix)
	}
	if shouldForceIdentityEncoding(route) {
		headers.Set("Accept-Encoding", "identity")
	}

	if origin := headers.Get("Origin"); origin != "" {
		headers.Set("Origin", s.rewriteRequestURLValue(origin, proxyOrigin, route))
	}

	if referer := headers.Get("Referer"); referer != "" {
		headers.Set("Referer", s.rewriteRequestURLValue(referer, proxyOrigin, route))
	}

	return headers
}

func shouldUseUpstreamOriginHeaders(route *routeContext) bool {
	if route == nil {
		return false
	}

	switch route.TargetURL.Path {
	case "/api/v1/user/order/checkout":
		return true
	default:
		return false
	}
}

func shouldForceIdentityEncoding(route *routeContext) bool {
	requestPath := strings.ToLower(route.TargetURL.Path)
	if shouldRewriteJSRoute(route) {
		return true
	}
	if requestPath == "" || requestPath == "/" {
		return true
	}
	if strings.HasPrefix(requestPath, "/api/") {
		return shouldRewriteJSONRoute(route)
	}

	switch strings.ToLower(path.Ext(requestPath)) {
	case ".html", ".htm":
		return true
	case ".css", ".js", ".mjs", ".svg", ".ico", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".woff", ".woff2", ".ttf", ".otf", ".eot", ".map", ".json", ".txt", ".xml":
		return false
	}

	return false
}

func (s *server) rewriteRequestURLValue(value, proxyOrigin string, route *routeContext) string {
	if value == "" {
		return value
	}

	if publicTarget := s.extractAllowedPublicTarget(value, proxyOrigin); publicTarget != nil {
		return publicTarget.String()
	}

	if strings.HasPrefix(value, proxyOrigin) {
		return route.Target.String() + value[len(proxyOrigin):]
	}

	return value
}

func (s *server) extractAllowedPublicTarget(value, proxyOrigin string) *url.URL {
	parsedURL := getSameProxyURL(value, proxyOrigin)
	if parsedURL == nil {
		return nil
	}

	route, err := s.parsePrefixedRoute(parsedURL.RequestURI())
	if err != nil || route == nil {
		return nil
	}
	if route.RedirectLocation != "" {
		redirectRoute, redirectErr := s.parsePrefixedRoute(route.RedirectLocation)
		if redirectErr != nil || redirectRoute == nil || redirectRoute.RedirectLocation != "" {
			return nil
		}
		route = redirectRoute
	}
	return route.TargetURL
}

func (s *server) buildResponseHeaders(upstreamRes *http.Response, rewriteBody bool, route *routeContext) http.Header {
	headers := cloneHeader(upstreamRes.Header)
	headers.Del("Content-Security-Policy")
	headers.Del("Content-Security-Policy-Report-Only")
	headers.Del("Strict-Transport-Security")
	headers.Del("Clear-Site-Data")

	if location := headers.Get("Location"); location != "" {
		headers.Set("Location", s.rewriteLocationHeader(location, route))
	}

	if setCookies := headers.Values("Set-Cookie"); len(setCookies) > 0 {
		headers.Del("Set-Cookie")
		for _, cookie := range setCookies {
			headers.Add("Set-Cookie", setCookieDomainPattern.ReplaceAllString(cookie, ""))
		}
	}

	if rewriteBody {
		headers.Del("Content-Length")
		headers.Del("Content-Encoding")
		headers.Del("Transfer-Encoding")
		headers.Del("Etag")
		headers.Del("Content-Md5")
	}

	return headers
}

func (s *server) rewriteLocationHeader(location string, route *routeContext) string {
	locationURL, err := url.Parse(location)
	if err != nil {
		return location
	}

	nextURL := route.Target.ResolveReference(locationURL)
	if !s.isAllowedTargetURL(nextURL) {
		return location
	}

	if route.RouteMode == "root" && nextURL.Scheme == s.cfg.DefaultTargetBase.Scheme && nextURL.Host == s.cfg.DefaultTargetBase.Host {
		return nextURL.RequestURI() + nextURL.Fragment
	}
	if route.RouteMode == "entry" && nextURL.Scheme == s.cfg.DefaultTargetBase.Scheme && nextURL.Host == s.cfg.DefaultTargetBase.Host {
		return canonicalProxyPath + nextURL.RequestURI() + nextURL.Fragment
	}

	return buildTargetPrefix(originURL(nextURL)) + nextURL.RequestURI() + nextURL.Fragment
}

func (s *server) rewriteTextBody(body, proxyOrigin string, route *routeContext) string {
	if !s.bodyMayContainAllowedHost(body) {
		return body
	}

	placeholderIndex := 0
	replacements := make(map[string]replacementEntry)

	rememberReplacement := func(match, replacement string) string {
		if replacement == "" {
			return match
		}

		if existing, ok := replacements[match]; ok {
			return existing.Placeholder
		}

		placeholder := fmt.Sprintf("__PUBLIC_ORIGIN_%d__", placeholderIndex)
		placeholderIndex++
		replacements[match] = replacementEntry{
			Placeholder: placeholder,
			Value:       replacement,
		}
		return placeholder
	}

	rewritten := body
	rewritten = replaceMatches(rewritten, escapedAbsoluteURLPattern, func(groups []string) string {
		publicOrigin := s.getPublicOriginForMatch(groups[1]+":", groups[2], proxyOrigin, route)
		if publicOrigin == "" {
			return groups[0]
		}
		return rememberReplacement(groups[0], strings.ReplaceAll(publicOrigin, "/", `\/`))
	})
	rewritten = replaceMatches(rewritten, absoluteURLPattern, func(groups []string) string {
		publicOrigin := s.getPublicOriginForMatch(groups[1]+":", groups[2], proxyOrigin, route)
		if publicOrigin == "" {
			return groups[0]
		}
		return rememberReplacement(groups[0], publicOrigin)
	})
	rewritten = replaceMatches(rewritten, escapedProtoRelativeURL, func(groups []string) string {
		publicOrigin := s.getPublicOriginForMatch(route.Target.Scheme, groups[3], proxyOrigin, route)
		if publicOrigin == "" {
			return groups[0]
		}
		return rememberReplacement(groups[0], groups[1]+strings.ReplaceAll(publicOrigin, "/", `\/`))
	})
	rewritten = replaceMatches(rewritten, protoRelativeURL, func(groups []string) string {
		publicOrigin := s.getPublicOriginForMatch(route.Target.Scheme, groups[3], proxyOrigin, route)
		if publicOrigin == "" {
			return groups[0]
		}
		return rememberReplacement(groups[0], groups[1]+publicOrigin)
	})

	for _, replacement := range replacements {
		rewritten = strings.ReplaceAll(rewritten, replacement.Placeholder, replacement.Value)
	}

	return rewritten
}

func (s *server) getPublicOriginForMatch(scheme, host, proxyOrigin string, route *routeContext) string {
	targetURL, err := url.Parse(fmt.Sprintf("%s//%s", scheme, host))
	if err != nil {
		return ""
	}

	targetBase, err := s.createTargetBase(targetURL)
	if err != nil {
		return ""
	}

	return s.getPublicOriginForTarget(targetBase, proxyOrigin, route)
}

func (s *server) getPublicOriginForTarget(targetBase *url.URL, proxyOrigin string, route *routeContext) string {
	if route.RouteMode == "entry" && sameOrigin(targetBase, s.cfg.DefaultTargetBase) {
		return proxyOrigin + canonicalProxyPath
	}
	if route.RouteMode == "root" && sameOrigin(targetBase, s.cfg.DefaultTargetBase) {
		return proxyOrigin
	}
	if route.RouteMode == "prefixed" && sameOrigin(targetBase, route.Target) {
		return route.PublicBase
	}
	return proxyOrigin + buildTargetPrefix(targetBase)
}

func (s *server) isResponseCacheCandidate(r *http.Request, route *routeContext) bool {
	if r.Method != http.MethodGet {
		return false
	}

	if isSharedCacheableRoute(route) {
		return true
	}

	if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		return false
	}
	requestPath := route.TargetURL.Path
	if strings.HasPrefix(requestPath, "/api/") {
		return false
	}
	if strings.Contains(requestPath, "/payment/") || strings.Contains(requestPath, "/order/") || strings.Contains(requestPath, "/passport/") {
		return false
	}
	if requestPath == "/" || requestPath == "" {
		return true
	}

	ext := strings.ToLower(path.Ext(requestPath))
	switch ext {
	case ".html", ".htm", ".css", ".js", ".mjs", ".svg", ".ico", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".woff", ".woff2", ".ttf", ".otf", ".eot", ".map", ".json", ".txt", ".xml":
		return true
	default:
		return false
	}
}

func (s *server) shouldStoreResponseCache(upstreamRes *http.Response, route *routeContext) bool {
	if upstreamRes.StatusCode != http.StatusOK {
		return false
	}
	if len(upstreamRes.Header.Values("Set-Cookie")) > 0 {
		return false
	}
	if contentLength := upstreamRes.Header.Get("Content-Length"); contentLength != "" {
		if size, err := strconv.Atoi(contentLength); err == nil && size > responseCacheMaxObj {
			return false
		}
	}

	ext := strings.ToLower(path.Ext(route.TargetURL.Path))
	contentType := strings.ToLower(upstreamRes.Header.Get("Content-Type"))
	if isSharedConfigRoute(route) {
		return strings.Contains(contentType, "application/json")
	}
	if route.TargetURL.Path == "/" || route.TargetURL.Path == "" {
		return strings.Contains(contentType, "text/html")
	}
	switch ext {
	case ".html", ".htm", ".css", ".js", ".mjs", ".svg", ".ico", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".woff", ".woff2", ".ttf", ".otf", ".eot", ".map", ".json", ".txt", ".xml":
		return true
	}

	return strings.HasPrefix(contentType, "image/") ||
		strings.Contains(contentType, "text/html") ||
		strings.Contains(contentType, "text/css") ||
		strings.Contains(contentType, "application/javascript") ||
		strings.Contains(contentType, "font/")
}

func (s *server) buildResponseCacheKey(route *routeContext) string {
	return route.PublicBase + "|" + route.TargetURL.String()
}

func (s *server) getCachedResponse(cacheKey string) (*responseCacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneResponseCacheLocked(time.Now())
	entry, ok := s.responseCache[cacheKey]
	if !ok {
		return nil, false
	}

	entry.LastAccess = time.Now()
	s.responseCache[cacheKey] = entry

	return &responseCacheEntry{
		StatusCode: entry.StatusCode,
		Header:     cloneHeader(entry.Header),
		Body:       append([]byte(nil), entry.Body...),
		ExpiresAt:  entry.ExpiresAt,
		LastAccess: entry.LastAccess,
		Size:       entry.Size,
	}, true
}

func (s *server) storeCachedResponse(cacheKey string, statusCode int, header http.Header, body []byte) {
	size := len(body)
	if size == 0 || size > responseCacheMaxObj {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.pruneResponseCacheLocked(now)

	if existing, ok := s.responseCache[cacheKey]; ok {
		s.responseCacheBytes -= existing.Size
		delete(s.responseCache, cacheKey)
	}

	for (len(s.responseCache) >= responseCacheMaxNum || s.responseCacheBytes+size > responseCacheMaxMem) && len(s.responseCache) > 0 {
		oldestKey := ""
		var oldestTime time.Time
		for key, entry := range s.responseCache {
			if oldestKey == "" || entry.LastAccess.Before(oldestTime) {
				oldestKey = key
				oldestTime = entry.LastAccess
			}
		}
		if oldestKey == "" {
			break
		}
		s.responseCacheBytes -= s.responseCache[oldestKey].Size
		delete(s.responseCache, oldestKey)
	}

	s.responseCache[cacheKey] = responseCacheEntry{
		StatusCode: statusCode,
		Header:     cloneHeader(header),
		Body:       append([]byte(nil), body...),
		ExpiresAt:  now.Add(responseCacheTTL),
		LastAccess: now,
		Size:       size,
	}
	s.responseCacheBytes += size
}

func (s *server) rememberContextualTarget(pathKey string, targetBase *url.URL) {
	if pathKey == "" || sameOrigin(targetBase, s.cfg.DefaultTargetBase) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneContextualTargetCacheLocked(time.Now())
	s.contextCache[pathKey] = contextualTargetEntry{
		TargetOrigin: targetBase.String(),
		ExpiresAt:    time.Now().Add(contextTTL),
	}
}

func (s *server) getCachedContextualTarget(pathKey string) *url.URL {
	if pathKey == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneContextualTargetCacheLocked(time.Now())
	entry, ok := s.contextCache[pathKey]
	if !ok {
		return nil
	}

	targetURL, err := url.Parse(entry.TargetOrigin)
	if err != nil {
		delete(s.contextCache, pathKey)
		return nil
	}

	return targetURL
}

func (s *server) pruneContextualTargetCacheLocked(now time.Time) {
	for key, entry := range s.contextCache {
		if !entry.ExpiresAt.After(now) {
			delete(s.contextCache, key)
		}
	}
}

func (s *server) pruneResponseCacheLocked(now time.Time) {
	for key, entry := range s.responseCache {
		if !entry.ExpiresAt.After(now) {
			s.responseCacheBytes -= entry.Size
			delete(s.responseCache, key)
		}
	}
	if s.responseCacheBytes < 0 {
		s.responseCacheBytes = 0
	}
}

func buildTargetPrefix(targetBase *url.URL) string {
	return canonicalProxyPath + "/" + targetBase.Scheme + "/" + targetBase.Host
}

func buildPathKey(pathname, rawQuery string) string {
	if rawQuery == "" {
		return pathname
	}
	return pathname + "?" + rawQuery
}

func getSameProxyURL(value, proxyOrigin string) *url.URL {
	if value == "" {
		return nil
	}

	parsedURL, err := url.Parse(value)
	if err != nil {
		return nil
	}
	if parsedURL.Scheme+"://"+parsedURL.Host != proxyOrigin {
		return nil
	}
	return parsedURL
}

func getProxyProtocol(r *http.Request) string {
	if forwardedProto := r.Header.Get("X-Forwarded-Proto"); forwardedProto != "" {
		return strings.TrimSpace(strings.Split(forwardedProto, ",")[0])
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func getProxyOrigin(r *http.Request) string {
	return getProxyProtocol(r) + "://" + r.Host
}

func getProxyPort(r *http.Request) string {
	if _, port, err := net.SplitHostPort(r.Host); err == nil && port != "" {
		return port
	}
	if getProxyProtocol(r) == "https" {
		return "443"
	}
	return "80"
}

func targetPort(target *url.URL) string {
	if target == nil {
		return "80"
	}
	if _, port, err := net.SplitHostPort(target.Host); err == nil && port != "" {
		return port
	}
	if strings.EqualFold(target.Scheme, "https") {
		return "443"
	}
	return "80"
}

func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func appendForwardedFor(existingValue, remoteAddress string) string {
	if remoteAddress == "" {
		return existingValue
	}
	if existingValue == "" {
		return remoteAddress
	}
	return existingValue + ", " + remoteAddress
}

func getDocumentBaseHref(r *http.Request, proxyOrigin string) string {
	return proxyOrigin + r.URL.Path
}

func shouldRewriteBody(contentType string, route *routeContext) bool {
	for _, prefix := range textContentTypes {
		if strings.Contains(contentType, prefix) {
			return true
		}
	}

	if strings.Contains(contentType, "javascript") && shouldRewriteJSRoute(route) {
		return true
	}

	if strings.Contains(contentType, "application/json") && shouldRewriteJSONRoute(route) {
		return true
	}

	return false
}

func shouldRewriteJSONRoute(route *routeContext) bool {
	switch route.TargetURL.Path {
	case "/api/config", "/api/v1/guest/comm/config", "/api/v1/user/getSubscribe":
		return true
	default:
		return false
	}
}

func shouldRewriteJSRoute(route *routeContext) bool {
	if route == nil {
		return false
	}

	requestPath := strings.ToLower(route.TargetURL.Path)
	return strings.HasPrefix(requestPath, "/theme/aurora/static/js/chunk-7f630ca2.") &&
		strings.HasSuffix(requestPath, ".js")
}

func isSharedConfigRoute(route *routeContext) bool {
	if route == nil {
		return false
	}

	switch route.TargetURL.Path {
	case "/api/config", "/api/v1/guest/comm/config":
		return true
	default:
		return false
	}
}

func isStaticAssetPath(requestPath string) bool {
	switch strings.ToLower(path.Ext(requestPath)) {
	case ".html", ".htm", ".css", ".js", ".mjs", ".svg", ".ico", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".woff", ".woff2", ".ttf", ".otf", ".eot", ".map", ".json", ".txt", ".xml":
		return true
	default:
		return false
	}
}

func isSharedCacheableRoute(route *routeContext) bool {
	if route == nil {
		return false
	}

	requestPath := route.TargetURL.Path
	if requestPath == "/" || requestPath == "" {
		return true
	}
	if isSharedConfigRoute(route) {
		return true
	}
	if strings.HasPrefix(requestPath, "/api/") {
		return false
	}
	if strings.Contains(requestPath, "/payment/") || strings.Contains(requestPath, "/order/") || strings.Contains(requestPath, "/passport/") {
		return false
	}
	return isStaticAssetPath(requestPath)
}

func shouldDropSensitiveRequestHeaders(route *routeContext) bool {
	if route == nil {
		return false
	}
	return isSharedCacheableRoute(route)
}

func maskProxyURL(rawURL string) string {
	if rawURL == "" {
		return "(empty)"
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "(invalid)"
	}
	if parsedURL.User != nil {
		username := parsedURL.User.Username()
		if username != "" {
			if _, hasPassword := parsedURL.User.Password(); hasPassword {
				parsedURL.User = url.UserPassword("***", "***")
			} else {
				parsedURL.User = url.User("***")
			}
		}
	}
	return parsedURL.String()
}

func rewriteEnvServerURL(body, publicBase string) string {
	if publicBase == "" || !strings.Contains(body, "serverUrl") {
		return body
	}

	replacement := fmt.Sprintf("serverUrl: '%s'", publicBase)
	return serverURLPattern.ReplaceAllString(body, replacement)
}

func rewriteOrderPaymentBundle(body string) string {
	oldCode := `window.location.href=n`
	newCode := `function(){var e=window.open("about:blank","_blank"),a=function(){setTimeout(function(){Object(c["f"])(t.orderData.trade_no).then(function(e){if(3===Number(e.data)){var a="/stage/order/info?id="+encodeURIComponent(t.orderData.trade_no)+"&paid=1&t="+Date.now();t.$message.success(t.$t("支付成功")),window.location.hash="#"+a,t.getOrderData();return}a()})["catch"](function(){a()})},3000)};if(e){try{e.opener=null,e.location.href=n}catch(r){e.location=n}a();return}window.location.href=n}()`
	if !strings.Contains(body, oldCode) {
		return body
	}
	return strings.Replace(body, oldCode, newCode, 1)
}

func rewriteHTMLRootRelativeAttrs(body string, route *routeContext) string {
	if route == nil || route.RouteMode != "prefixed" || route.Prefix == "" {
		return body
	}
	if !strings.Contains(body, "=\"/") && !strings.Contains(body, "='/") {
		return body
	}

	return rootRelativeAttrPattern.ReplaceAllStringFunc(body, func(match string) string {
		groups := rootRelativeAttrPattern.FindStringSubmatch(match)
		if groups == nil {
			return match
		}

		attrPrefix := groups[1]
		relativePath := groups[2]
		if strings.HasPrefix(relativePath, "/") ||
			strings.HasPrefix(relativePath, strings.TrimPrefix(canonicalProxyPath, "/")) ||
			strings.HasPrefix(relativePath, "http://") ||
			strings.HasPrefix(relativePath, "https://") {
			return match
		}

		return attrPrefix + route.Prefix + "/" + relativePath
	})
}

func replaceMatches(input string, re *regexp.Regexp, replacer func([]string) string) string {
	return re.ReplaceAllStringFunc(input, func(match string) string {
		groups := re.FindStringSubmatch(match)
		if groups == nil {
			return match
		}
		return replacer(groups)
	})
}

func cloneHeader(header http.Header) http.Header {
	clone := make(http.Header, len(header))
	for key, values := range header {
		copied := make([]string, len(values))
		copy(copied, values)
		clone[key] = copied
	}
	return clone
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func originURL(value *url.URL) *url.URL {
	return &url.URL{
		Scheme: value.Scheme,
		Host:   value.Host,
	}
}

func cloneURL(value *url.URL) *url.URL {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return left.Scheme == right.Scheme && left.Host == right.Host
}

func splitLowerCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(strings.ToLower(part))
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func normalizeHostSuffixes(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(strings.ToLower(value))
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func loadFileConfig() (fileConfig, string, error) {
	if configPath, ok := lookupNonEmptyEnv("CONFIG_FILE"); ok {
		cfg, err := readFileConfig(configPath)
		if err != nil {
			return fileConfig{}, "", err
		}
		return cfg, configPath, nil
	}

	candidates := make([]string, 0, 2)
	if executablePath, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executablePath), "config.json"))
	}
	if workingDir, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(workingDir, "config.json"))
	}

	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}

		absolutePath, err := filepath.Abs(candidate)
		if err == nil {
			candidate = absolutePath
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}

		if _, err := os.Stat(candidate); err == nil {
			cfg, readErr := readFileConfig(candidate)
			if readErr != nil {
				return fileConfig{}, "", readErr
			}
			return cfg, candidate, nil
		} else if !os.IsNotExist(err) {
			return fileConfig{}, "", fmt.Errorf("cannot read config file %s: %w", candidate, err)
		}
	}

	return fileConfig{}, "", nil
}

func readFileConfig(configPath string) (fileConfig, error) {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return fileConfig{}, fmt.Errorf("cannot read config file %s: %w", configPath, err)
	}

	var cfg fileConfig
	if err := json.Unmarshal(content, &cfg); err != nil {
		return fileConfig{}, fmt.Errorf("invalid config file %s: %w", configPath, err)
	}

	return cfg, nil
}

func lookupNonEmptyEnv(key string) (string, bool) {
	value, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}

	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}

	return value, true
}

func lookupEnvBool(key string) (bool, bool, error) {
	value, ok := os.LookupEnv(key)
	if !ok {
		return false, false, nil
	}

	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true, true, nil
	case "0", "false", "no", "off":
		return false, true, nil
	default:
		return false, true, fmt.Errorf("invalid %s: %s", key, value)
	}
}

func getEnvBool(key string, fallback bool) bool {
	value, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}

	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func (s *server) bodyMayContainAllowedHost(body string) bool {
	body = strings.ToLower(body)
	for _, suffix := range s.cfg.AllowedHostSuffixes {
		if strings.Contains(body, suffix) {
			return true
		}
	}
	return false
}

func toAllowedTargetText(suffixes []string) []string {
	result := make([]string, 0, len(suffixes))
	for _, suffix := range suffixes {
		result = append(result, suffix+" and its subdomains")
	}
	return result
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}
