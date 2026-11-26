// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

// httpMethod enumerates the HTTP verbs the router recognizes. methodCount is
// never a real verb — it is the size of the enum, used to size the per-method
// tables. It gets its own const block so these iota values stay separate from
// the plain string constants below (mixing an iota enum with unrelated consts
// makes exhaustive-switch tooling misread every switch that touches them).
const (
	// methodGet never reads a request body.
	methodGet httpMethod = iota
	// methodHead reuses the GET route unless registered.
	methodHead
	// methodPost binds the request body by Content-Type.
	methodPost
	// methodPut binds the request body by Content-Type.
	methodPut
	// methodPatch binds the request body by Content-Type.
	methodPatch
	// methodDelete never reads a request body, like GET.
	methodDelete
	// methodOptions is answered automatically unless registered.
	methodOptions
	// methodCount is not a verb: it is the size of the enum and sizes the
	// per-method tables.
	methodCount
)

// Internal string constants shared by more than one file, kept in one place so
// these magic values are single-sourced rather than scattered across the code.
// A constant used by only one file belongs in that file, beside the code it
// serves.
const (
	// empty is the zero string, so comparisons all read the same way.
	empty = ""
	// slash separates route path segments.
	slash = "/"
	// colon binds a listen port, a file:line position key and a variable
	// route parameter marker, as in /user/:id.
	colon = ":"
	// vary names the request headers a response depended on.
	vary = "Vary"
	// headerComma is the canonical joiner for multi-value header lists.
	headerComma = ", "
	// comma separates list items inside a single header value.
	comma = ","
	// asterisk is the wildcard token in CORS lists and Accept-Encoding.
	asterisk = "*"
	// textPlainCharset is the default body type for plain text and error
	// replies.
	textPlainCharset = "text/plain; charset=utf-8"
	// applicationJson is the exact media type of JSON payloads.
	applicationJson = "application/json"
	// applicationJsonCharset is the JSON media type with charset, what the
	// framework answers with.
	applicationJsonCharset = "application/json; charset=utf-8"
	// contentType names the media type of payload.
	contentType = "Content-Type"
	// allow lists the methods a path really supports.
	allow = "Allow"
)
