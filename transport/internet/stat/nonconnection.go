package stat

import (
	"github.com/xtls/xray-core/features/stats"
)

type NoCopyConnection struct {
	Connection
	ReadCounter  stats.Counter
	WriteCounter stats.Counter
}

func (c *NoCopyConnection) Read(b []byte) (int, error) {
	nBytes, err := c.Connection.Read(b)
	if c.ReadCounter != nil {
		c.ReadCounter.Add(int64(nBytes))
	}

	return nBytes, err
}

func (c *NoCopyConnection) Write(b []byte) (int, error) {
	nBytes, err := c.Connection.Write(b)
	if c.WriteCounter != nil {
		c.WriteCounter.Add(int64(nBytes))
	}
	return nBytes, err
}
