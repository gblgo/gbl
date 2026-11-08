// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

const (
	methodGet Method = iota
	methodPost
	methodPut
	methodDelete
	methodCount
	null                          = ""
	slash                         = "/"
	colon                         = ":"
	point                         = "."
	comma                         = ","
	asterisk                      = "*"
	leftCurlyBrace                = "{"
	rightCurlyBrace               = "}"
	vary                          = "Vary"
	headerTrue                    = "true"
	headerComma                   = ", "
	applicationIni                = "application.ini"
	headerOrigin                  = "Origin"
	textPlain                     = "text/plain"
	multipartFormData             = "multipart/form-data"
	xWwwFormUrlencoded            = "application/x-www-form-urlencoded"
	applicationJson               = "application/json"
	applicationNoJson             = "application/"
	plusSignJson                  = "+json"
	contentType                   = "Content-Type"
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
