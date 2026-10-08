package main

import (
	"bytes"
	stdgzip "compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	fastgzip "github.com/klauspost/compress/gzip"
)

type reusableGzipWriter interface {
	io.WriteCloser
	Reset(io.Writer)
}

// Compare pooled encoders on identical saved API JSON, independently of HTTP load.
func BenchmarkResponseGzip(b *testing.B) {
	dir := os.Getenv("ISUPIPE_COMPRESSION_SAMPLES")
	if dir == "" {
		b.Skip("use task check-response-compression with captured API JSON")
	}
	encoders := []struct {
		name string
		new  func(io.Writer) (reusableGzipWriter, error)
	}{
		{"stdlib-1", func(w io.Writer) (reusableGzipWriter, error) { return stdgzip.NewWriterLevel(w, 1) }},
		{"klauspost-1", func(w io.Writer) (reusableGzipWriter, error) { return fastgzip.NewWriterLevel(w, 1) }},
		{"klauspost-2", func(w io.Writer) (reusableGzipWriter, error) { return fastgzip.NewWriterLevel(w, 2) }},
	}
	for _, name := range []string{"search", "livecomment", "reaction"} {
		data, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			b.Fatal(err)
		}
		for _, encoder := range encoders {
			b.Run(name+"/"+encoder.name, func(b *testing.B) {
				var buffer bytes.Buffer
				writer, err := encoder.new(&buffer)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := writer.Write(data); err != nil {
					b.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					b.Fatal(err)
				}
				reader, err := stdgzip.NewReader(bytes.NewReader(buffer.Bytes()))
				if err != nil {
					b.Fatal(err)
				}
				decoded, err := io.ReadAll(reader)
				reader.Close()
				if err != nil || !bytes.Equal(decoded, data) {
					b.Fatalf("decoded JSON differs: %v", err)
				}
				b.SetBytes(int64(len(data)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					buffer.Reset()
					writer.Reset(&buffer)
					if _, err := writer.Write(data); err != nil {
						b.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(buffer.Len()), "encoded-B/op")
			})
		}
	}
}
