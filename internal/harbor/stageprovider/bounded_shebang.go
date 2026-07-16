package stageprovider

import (
	"bufio"
	"io"
)

func readBoundedShebangLine(reader io.Reader, limit int) ([]byte, error) {
	line, err := bufio.NewReaderSize(io.LimitReader(reader, int64(limit)+1), limit+1).ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	return line[:len(line)-1], nil
}
