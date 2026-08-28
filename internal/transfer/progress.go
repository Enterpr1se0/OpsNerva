package transfer

import "io"

// Progress is the common byte-level contract for file transfers. Total may be
// zero only when the source length is not known before streaming starts.
type Progress struct {
	Transferred int64 `json:"transferred"`
	Total       int64 `json:"total"`
}

type Reporter func(Progress)

type Writer struct {
	writer      io.Writer
	total       int64
	written     int64
	next        int64
	step        int64
	lastEmitted int64
	report      Reporter
}

func NewWriter(writer io.Writer, total int64, report Reporter) *Writer {
	step := total / 100
	if step < 256*1024 {
		step = 256 * 1024
	}
	progress := &Writer{writer: writer, total: total, next: step, step: step, report: report}
	if report != nil {
		report(Progress{Total: total})
	}
	return progress
}

func (w *Writer) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.written += int64(n)
	if w.report != nil && (w.written >= w.next || w.total > 0 && w.written == w.total) {
		w.emit()
		for w.next <= w.written {
			w.next += w.step
		}
	}
	return n, err
}

func (w *Writer) Finish() {
	if w.report != nil && w.lastEmitted != w.written {
		w.emit()
	}
}

func (w *Writer) emit() {
	w.report(Progress{Transferred: w.written, Total: w.total})
	w.lastEmitted = w.written
}
