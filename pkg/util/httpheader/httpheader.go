package httpheader

import (
	"net/http"
	"net/textproto"
	"strings"

	"github.com/megaease/easegateway/pkg/util/stringtool"
)

type (
	// HTTPHeader is the wrapper of http.Header with more abilities.
	HTTPHeader struct {
		h http.Header
	}

	// AdaptSpec describes rules for adapting.
	AdaptSpec struct {
		Del []string `yaml:"del" jsonschema:"omitempty,uniqueItems=true"`

		// NOTE: Set and Add allow empty value.
		Set map[string]string `yaml:"set" jsonschema:"omitempty"`
		Add map[string]string `yaml:"add" jsonschema:"omitempty"`
	}
)

// New creates an HTTPHeader.
func New(src http.Header) *HTTPHeader {
	return &HTTPHeader{h: src}
}

// Reset resets internal src http.Header.
func (h *HTTPHeader) Reset(src http.Header) {
	for key := range h.h {
		delete(h.h, key)
	}

	for key, values := range src {
		h.h[key] = values
	}
}

// Copy copies HTTPHeader to a whole new HTTPHeader.
func (h *HTTPHeader) Copy() *HTTPHeader {
	n := make(http.Header)
	for key, values := range h.h {
		copyValues := make([]string, len(values))
		copy(copyValues, values)
		n[key] = copyValues
	}

	return &HTTPHeader{h: n}
}

// Std returns internal Header of standard library.
func (h *HTTPHeader) Std() http.Header {
	return h.h
}

// Add adds the key value pair.
func (h *HTTPHeader) Add(key, value string) {
	h.h.Add(key, value)
}

// Get gets the FIRST value by the key.
func (h *HTTPHeader) Get(key string) string {
	return h.h.Get(key)
}

// GetAll gets all values of the key.
func (h *HTTPHeader) GetAll(key string) []string {
	key = textproto.CanonicalMIMEHeaderKey(key)
	return h.h[key]
}

// Set the key value pair of headers.
func (h *HTTPHeader) Set(key, value string) {
	h.h.Set(key, value)
}

// Del deletes the key value pair by the key.
func (h *HTTPHeader) Del(key string) {
	h.h.Del(key)
}

// VisitAll call fn with every key value pair.
func (h *HTTPHeader) VisitAll(fn func(key, value string)) {
	for key, values := range h.h {
		for _, value := range values {
			fn(key, value)
		}
	}
}

// Dump dumps HTTPHeader in RFC format.
func (h *HTTPHeader) Dump() string {
	var headers []string
	h.VisitAll(func(key, value string) {
		headers = append(headers, stringtool.Cat(key, ": ", value))
	})

	return strings.Join(headers, "\r\n")
}

// AddFrom adds values from another HTTPHeader.
func (h *HTTPHeader) AddFrom(src *HTTPHeader) {
	for key, values := range src.h {
		for _, value := range values {
			h.h.Add(key, value)
		}
	}
}

// AddFromStd wraps AddFrom by replacing
// the paramenter type *HTTPHeader with standard http.Header.
func (h *HTTPHeader) AddFromStd(src http.Header) {
	h.AddFrom(New(src))
}

// SetFrom sets values from another HTTPHeader.
func (h *HTTPHeader) SetFrom(src *HTTPHeader) {
	for key, values := range src.h {
		for _, value := range values {
			h.h.Set(key, value)
		}
	}
}

// SetFromStd wraps Setfrom by replacing
// the paramenter type *HTTPHeader with standard http.Header.
func (h *HTTPHeader) SetFromStd(src http.Header) {
	h.SetFrom(New(src))
}

// Adapt adapts HTTPHeader according to AdaptSpec.
func (h *HTTPHeader) Adapt(as *AdaptSpec) {
	for _, key := range as.Del {
		h.Del(key)
	}

	for key, value := range as.Set {
		h.Set(key, value)
	}

	for key, value := range as.Add {
		h.Add(key, value)
	}
}
