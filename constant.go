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
	methodGet httpMethod = iota
	methodHead
	methodPost
	methodPut
	methodPatch
	methodDelete
	methodOptions
	methodCount
)

// Package-wide internal string constants, kept in one place so these magic values
// are single-sourced rather than scattered across the code: delimiter/empty
// shortcuts, HTTP header names, media types and fixed error/response texts.
const (
	null                          = ""
	slash                         = "/"
	colon                         = ":"
	comma                         = ","
	asterisk                      = "*"
	leftCurlyBrace                = "{"
	rightCurlyBrace               = "}"
	vary                          = "Vary"
	headerTrue                    = "true"
	headerComma                   = ", "
	applicationIni                = "application.ini"
	startErrorLogFile             = "gbl-start-error.log"
	headerOrigin                  = "Origin"
	textPlainCharset              = "text/plain; charset=utf-8"
	multipartFormData             = "multipart/form-data"
	xWwwFormUrlencoded            = "application/x-www-form-urlencoded"
	applicationJson               = "application/json"
	applicationJsonCharset        = "application/json; charset=utf-8"
	applicationNoJson             = "application/"
	plusSignJson                  = "+json"
	contentType                   = "Content-Type"
	allow                         = "Allow"
	accessControlRequestMethod    = "Access-Control-Request-Method"
	accessControlAllowOrigin      = "Access-Control-Allow-Origin"
	accessControlAllowCredentials = "Access-Control-Allow-Credentials"
	accessControlExposeHeaders    = "Access-Control-Expose-Headers"
	accessControlRequestHeaders   = "Access-Control-Request-Headers"
	accessControlAllowMethods     = "Access-Control-Allow-Methods"
	accessControlAllowHeaders     = "Access-Control-Allow-Headers"
	accessControlMaxAge           = "Access-Control-Max-Age"
	corsOriginNotAllowed          = "CORS origin not allowed"
	corsMethodNotAllowed          = "CORS method not allowed"
	corsHeaderNotAllowed          = "CORS header not allowed"
	responseSerializationFailed   = "response serialization failed"
	unknownHTTPMethodPrefix       = "unknown http method: "
	injectParameterErrFormat      = "inject parameter %q: %w"
	jsonParameterErrFormat        = "JSON parameter %q: %w"
)
