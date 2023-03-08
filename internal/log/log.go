package log

import (
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	log_v1 "github.com/igsr5/proglog/api/v1"
	"github.com/pkg/errors"
)

type Log struct {
	mu sync.RWMutex

	Dir    string
	Config Config

	activeSegment *segment
	segments      []*segment
}

func NewLog(dir string, c Config) (*Log, error) {
	if c.Segment.MaxStoreBytes == 0 {
		c.Segment.MaxStoreBytes = 1024
	}
	if c.Segment.MaxIndexBytes == 0 {
		c.Segment.MaxIndexBytes = 1024
	}
	l := &Log{
		Dir:    dir,
		Config: c,
	}
	return l, l.setup()
}

func (l *Log) setup() error {
	files, err := os.ReadDir(l.Dir)
	if err != nil {
		return errors.Wrap(err, "failed to read dir")
	}
	var baseOffsets []uint64
	for _, file := range files {
		offStr := strings.TrimSuffix(
			file.Name(),
			path.Ext(file.Name()),
		)
		off, _ := strconv.ParseUint(offStr, 10, 0)
		baseOffsets = append(baseOffsets, off)
	}
	sort.Slice(baseOffsets, func(i, j int) bool {
		return baseOffsets[i] < baseOffsets[j]
	})
	for i := 0; i < len(baseOffsets); i += 2 {
		if err = l.newSegment(baseOffsets[i]); err != nil {
			return errors.Wrap(err, "failed newSegment")
		}
	}
	if l.segments == nil {
		if err = l.newSegment(l.Config.Segment.InitialOffset); err != nil {
			return errors.Wrap(err, "failed newSegment")
		}
	}

	return nil
}

func (l *Log) Append(record *log_v1.Record) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	highestOffset, err := l.highestOffset()
	if err != nil {
		return 0, errors.Wrap(err, "failed to get highestOffset")
	}

	if l.activeSegment.IsMax() {
		err = l.newSegment(highestOffset + 1)
		if err != nil {
			return 0, err
		}
	}

	off, err := l.activeSegment.Append(record)
	if err != nil {
		return 0, errors.Wrap(err, "failed to append record")
	}

	return off, nil
}

func (l *Log) Read(off uint64) (*log_v1.Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var s *segment
	for _, segment := range l.segments {
		if segment.baseOffset <= off && off < segment.nextOffset {
			s = segment
			break
		}
	}
	if s == nil || s.nextOffset <= off {
		return nil, fmt.Errorf("offset out of range: %d", off)
	}

	return s.Read(off)
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, segment := range l.segments {
		if err := segment.Close(); err != nil {
			return errors.Wrap(err, "failed to close segment")
		}
	}
	return nil
}

func (l *Log) Remove() error {
	if err := l.Close(); err != nil {
		return errors.Wrap(err, "failed to remove segment")
	}
	return os.RemoveAll(l.Dir)
}

func (l *Log) Reset() error {
	if err := l.Remove(); err != nil {
		return errors.Wrap(err, "failed to remove log")
	}
	return l.setup()
}

func (l *Log) highestOffset() (uint64, error) {
	off := l.segments[len(l.segments)-1].nextOffset
	if off == 0 {
		return 0, nil
	}

	return off - 1, nil
}

func (l *Log) LowestOffset() (uint64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.segments[0].baseOffset, nil
}

func (l *Log) HighestOffset() (uint64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.highestOffset()
}

func (l *Log) Truncate(lowest uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var segments []*segment
	for _, s := range l.segments {
		if s.nextOffset <= lowest+1 {
			if err := s.Remove(); err != nil {
				return errors.Wrap(err, "failed to remove segment")
			}
			continue
		}
		segments = append(segments, s)
	}
	l.segments = segments
	return nil
}

func (l *Log) Reader() io.Reader {
	l.mu.RLock()
	defer l.mu.RUnlock()
	readers := make([]io.Reader, len(l.segments))
	for i, segment := range l.segments {
		readers[i] = &originReader{segment.store, 0}
	}
	return io.MultiReader(readers...)
}

type originReader struct {
	*store
	off int64
}

func (o *originReader) Read(p []byte) (int, error) {
	n, err := o.ReadAt(p, o.off)
	o.off += int64(n)
	return n, err
}

func (l *Log) newSegment(baseOffset uint64) error {
	s, err := newSegment(l.Dir, baseOffset, l.Config)
	if err != nil {
		return errors.Wrap(err, "failed to create segment")
	}

	l.segments = append(l.segments, s)
	l.activeSegment = s

	return nil
}
