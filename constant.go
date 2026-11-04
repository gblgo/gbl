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
	null            = ""
	slash           = "/"
	colon           = ":"
	point           = "."
	leftCurlyBrace  = "{"
	rightCurlyBrace = "}"
	textPlain       = "text/plain"
	applicationJson = "application/json"
)
