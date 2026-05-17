package dialgo

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
)

var CookieDomains = []string{
	"https://dialpad.com",
	"https://accounts.google.com",
}

func newCookieJar(stored map[string]string) (*cookiejar.Jar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	for key, val := range stored {
		name, domain, ok := strings.Cut(key, "@")
		if !ok || name == "" || domain == "" || val == "" {
			continue
		}
		host := strings.TrimPrefix(domain, ".")
		u := &url.URL{Scheme: "https", Host: host, Path: "/"}
		jar.SetCookies(u, []*http.Cookie{{
			Name:   name,
			Value:  val,
			Domain: domain,
			Path:   "/",
		}})
	}
	return jar, nil
}

func exportCookies(jar http.CookieJar) map[string]string {
	out := map[string]string{}
	if jar == nil {
		return out
	}
	for _, origin := range CookieDomains {
		u, err := url.Parse(origin)
		if err != nil {
			continue
		}
		for _, c := range jar.Cookies(u) {
			domain := c.Domain
			if domain == "" {
				domain = u.Host
			}
			out[c.Name+"@"+domain] = c.Value
		}
	}
	return out
}

func hasCookie(jar http.CookieJar, name string) bool {
	if jar == nil {
		return false
	}
	for _, origin := range CookieDomains {
		u, err := url.Parse(origin)
		if err != nil {
			continue
		}
		for _, c := range jar.Cookies(u) {
			if c.Name == name {
				return true
			}
		}
	}
	return false
}
