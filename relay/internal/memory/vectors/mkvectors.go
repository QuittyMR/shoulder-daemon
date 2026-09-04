//go:build ignore

// mkvectors builds vectors.bin from a GloVe text file.
//
//	curl -LO https://huggingface.co/stanfordnlp/glove/resolve/main/glove.6B.zip
//	unzip -p glove.6B.zip glove.6B.100d.txt > glove.6B.100d.txt
//	go run mkvectors.go -in glove.6B.100d.txt -out vectors.bin -words 40000
//
// The input is Stanford's pre-trained GloVe vectors, which are published under
// the Open Data Commons Public Domain Dedication and Licence v1.0. The
// attribution and the exact provenance of the table in this directory are in
// NOTICE beside it.
//
// The file is written in the order the input has, which for GloVe is descending
// frequency, because the reader derives a word's rarity weight from its
// position. Each row is scaled to fill a signed byte: the error that introduces
// is under a percent of the row's magnitude, and cosine similarity between two
// such rows is indistinguishable from the same measure on the floats at the
// precision anybody ranks on.
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

const magic = "SDVEC01\n"

func main() {
	in := flag.String("in", "", "GloVe text file: one word and its dimensions per line")
	out := flag.String("out", "vectors.bin", "where to write the table")
	limit := flag.Int("words", 40000, "how many words to keep, from the most frequent")
	flag.Parse()
	if *in == "" {
		fmt.Fprintln(os.Stderr, "mkvectors: -in is required")
		os.Exit(2)
	}
	if err := run(*in, *out, *limit); err != nil {
		fmt.Fprintln(os.Stderr, "mkvectors:", err)
		os.Exit(1)
	}
}

func run(in, out string, limit int) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()

	var (
		words  []string
		scales []float32
		rows   [][]int8
		dims   int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() && len(words) < limit {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		word := fields[0]
		// Punctuation and single characters are dropped: the reader's
		// tokeniser cannot produce them, so their rows would be dead weight in
		// every binary this table ever ships in.
		if len([]rune(word)) < 2 || !isWord(word) {
			continue
		}
		if dims == 0 {
			dims = len(fields) - 1
		}
		if len(fields)-1 != dims {
			return fmt.Errorf("%q has %d dimensions, expected %d", word, len(fields)-1, dims)
		}

		values := make([]float64, dims)
		peak := 0.0
		for i, raw := range fields[1:] {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return fmt.Errorf("%q: %w", word, err)
			}
			values[i] = v
			if a := math.Abs(v); a > peak {
				peak = a
			}
		}
		if peak == 0 {
			continue
		}
		scale := peak / 127
		row := make([]int8, dims)
		for i, v := range values {
			row[i] = int8(math.Round(v / scale))
		}
		words = append(words, word)
		scales = append(scales, float32(scale))
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(words) == 0 {
		return fmt.Errorf("%s held no usable vectors", in)
	}

	var buf []byte
	buf = append(buf, magic...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(dims))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(words)))
	for _, w := range words {
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(w)))
		buf = append(buf, w...)
	}
	for _, s := range scales {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(s))
	}
	for _, row := range rows {
		for _, v := range row {
			buf = append(buf, byte(v))
		}
	}
	if err := os.WriteFile(out, buf, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %d words, %d dimensions, %.1f MB\n", out, len(words), dims, float64(len(buf))/(1<<20))
	return nil
}

// isWord keeps the rows a sentence can actually reach. GloVe's vocabulary is
// whatever survived tokenising a web crawl, so it is full of punctuation runs,
// numerals and fragments that no query will ever contain.
func isWord(s string) bool {
	letters := 0
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			letters++
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return letters > 0
}
