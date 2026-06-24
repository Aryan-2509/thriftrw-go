// Copyright (c) 2024 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package compile

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const generateTestIncludedThrift = `
namespace go shared

struct Shared {
  1: required string id
}
`

const generateTestMainThrift = `
namespace go main
namespace java com.example.main

include "included.thrift"

const i32 MaxRetries = 3
const string Greeting = "hello\n\"world\""
const list<i32> Primes = [2, 3, 5]
const map<string, i32> Scores = {"a": 1, "b": 2}

typedef i64 Timestamp
typedef list<string> StringList

enum Color {
  RED = 0,
  GREEN = 1,
  BLUE = 2
}

struct Point {
  1: required i32 x
  2: optional i32 y = 10
  3: optional Color color = Color.RED
  4: optional included.Shared shared
}

union Value {
  1: i32 intValue
  2: string strValue
}

exception NotFound {
  1: required string message
}

service Base {
  void ping()
}

service API extends Base {
  oneway void notify(1: string msg)
  Point getPoint(1: required i32 id) throws (1: NotFound notFound)
}
`

func writeGenerateTestFiles(t *testing.T) (dir, mainPath string) {
	t.Helper()
	dir = t.TempDir()

	mainPath = filepath.Join(dir, "main.thrift")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "included.thrift"), []byte(generateTestIncludedThrift), 0o644))
	require.NoError(t, os.WriteFile(mainPath, []byte(generateTestMainThrift), 0o644))
	return dir, mainPath
}

func typeNames(m *Module) []string {
	names := make([]string, 0, len(m.Types))
	for name := range m.Types {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func constantNames(m *Module) []string {
	names := make([]string, 0, len(m.Constants))
	for name := range m.Constants {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func serviceNames(m *Module) []string {
	names := make([]string, 0, len(m.Services))
	for name := range m.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestGenerateThriftFile_RoundTrip(t *testing.T) {
	dir, mainPath := writeGenerateTestFiles(t)

	original, err := Compile(mainPath)
	require.NoError(t, err, "compiling source thrift")

	// Generate into the same directory so the relative include still resolves.
	outPath := filepath.Join(dir, "out_main.thrift")
	require.NoError(t, original.GenerateThriftFile(outPath), "generating thrift file")

	generated, err := os.ReadFile(outPath)
	require.NoError(t, err)
	content := string(generated)

	t.Run("content_contains_expected_constructs", func(t *testing.T) {
		for _, want := range []string{
			"namespace go main",
			"namespace java com.example.main",
			`include "included.thrift"`,
			"const i32 MaxRetries = 3",
			`\n\"world\"`, // escaped newline + escaped quote in the string constant
			"typedef i64 Timestamp",
			"typedef list<string> StringList",
			"enum Color {",
			"RED = 0",
			"struct Point {",
			"1: required i32 x",
			"2: optional i32 y = 10",
			"3: optional Color color = Color.RED",
			"included.Shared shared",
			"union Value {",
			"exception NotFound {",
			"service Base {",
			"service API extends Base {",
			"oneway void notify(",
			"throws (",
			"void ping()",
		} {
			assert.Contains(t, content, want)
		}
	})

	t.Run("recompiles_to_equivalent_module", func(t *testing.T) {
		recompiled, err := Compile(outPath)
		require.NoError(t, err, "recompiling generated thrift")

		assert.Equal(t, typeNames(original), typeNames(recompiled), "type names")
		assert.Equal(t, constantNames(original), constantNames(recompiled), "constant names")
		assert.Equal(t, serviceNames(original), serviceNames(recompiled), "service names")
		assert.Equal(t, original.Namespaces, recompiled.Namespaces, "namespaces")
		assert.Equal(t, len(original.Includes), len(recompiled.Includes), "include count")

		// Spot-check a struct's field-level details survive the round trip.
		pt, ok := recompiled.Types["Point"].(*StructSpec)
		require.True(t, ok, "Point should be a struct")
		require.Len(t, pt.Fields, 4)
		assert.True(t, pt.Fields[0].Required, "field x should be required")
		require.NotNil(t, pt.Fields[1].Default, "field y should have a default")

		// The union's fields should not be required.
		union, ok := recompiled.Types["Value"].(*StructSpec)
		require.True(t, ok)
		for _, f := range union.Fields {
			assert.False(t, f.Required, "union field %s should not be required", f.Name)
		}
	})
}

func TestGenerateThriftFile_NilModule(t *testing.T) {
	var m *Module
	assert.Error(t, m.GenerateThriftFile(filepath.Join(t.TempDir(), "out.thrift")))
}

func TestModuleThriftIDL_Deterministic(t *testing.T) {
	_, mainPath := writeGenerateTestFiles(t)

	m, err := Compile(mainPath)
	require.NoError(t, err)

	first, err := m.thriftIDL()
	require.NoError(t, err)
	second, err := m.thriftIDL()
	require.NoError(t, err)

	assert.Equal(t, first, second, "generated output should be deterministic across runs")
}
