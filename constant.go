// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

// Package-wide internal constants, kept in one place so these magic values are
// single-sourced rather than scattered across the code. The first run defines the
// Method enum (methodGet..methodOptions, with methodCount as its size and never a
// real verb); the rest are shared string literals — delimiter/empty shortcuts,
// HTTP header names, media types and fixed error/response texts.
const (
	methodGet Method = iota
	methodHead
	methodPost
	methodPut
	methodPatch
	methodDelete
	methodOptions
	methodCount
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
)
