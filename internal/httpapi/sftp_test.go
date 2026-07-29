package httpapi

import (
	"bytes"
	"io"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
)

func TestSFTPUploadReaderPreservesSelectedTextEncoding(t *testing.T) {
	const content = "中文 text\r\n"
	tests := []struct {
		name     string
		encoding string
		decode   func([]byte) (string, error)
	}{
		{
			name:     "UTF-8",
			encoding: "utf-8",
			decode:   func(data []byte) (string, error) { return string(data), nil },
		},
		{
			name:     "UTF-16LE",
			encoding: "utf-16le",
			decode: func(data []byte) (string, error) {
				return unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM).NewDecoder().String(string(data))
			},
		},
		{
			name:     "UTF-16BE",
			encoding: "utf-16be",
			decode: func(data []byte) (string, error) {
				return unicode.UTF16(unicode.BigEndian, unicode.ExpectBOM).NewDecoder().String(string(data))
			},
		},
		{
			name:     "GB18030",
			encoding: "gb18030",
			decode: func(data []byte) (string, error) {
				return simplifiedchinese.GB18030.NewDecoder().String(string(data))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader, err := sftpUploadReader(bytes.NewBufferString(content), test.encoding)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := test.decode(data)
			if err != nil {
				t.Fatal(err)
			}
			if decoded != content {
				t.Fatalf("decoded content = %q, want %q", decoded, content)
			}
		})
	}
}

func TestSFTPUploadReaderRejectsUnknownEncoding(t *testing.T) {
	if _, err := sftpUploadReader(bytes.NewBufferString("text"), "unknown"); err == nil {
		t.Fatal("unknown encoding was accepted")
	}
}
