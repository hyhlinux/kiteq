package memory

import (
	"bufio"
	"encoding/binary"
	log "github.com/blackbeans/log4go"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
)

type ChunkFlag uint8

func (self ChunkFlag) String() string {

	switch self {
	case NORMAL:
		return "NORMAL"
	case DELETE:
		return "DELETE"
	case EXPIRED:
		return "EXPIRED"
	}
	return ""
}

const (
	MAX_SEGMENT_SIZE = 64 * 1024 * 1024 //最大的分段大仙
	// MAX_CHUNK_SIZE      = 64 * 1024        //最大的chunk
	SEGMENT_PREFIX                = "segment-"
	SEGMENT_IDX_SUFFIX            = ".idx"
	SEGMENT_DATA_SUFFIX           = ".data"
	CHUNK_HEADER                  = 4 + 4 + 8 + 1 //|length 4byte|checksum 4byte|id 8byte|flag 1byte| data variant|
	NORMAL              ChunkFlag = 'n'
	DELETE              ChunkFlag = 'd'
	EXPIRED             ChunkFlag = 'e'
)

//消息文件
type Segment struct {
	path     string
	name     string //basename_0000000000
	rf       *os.File
	wf       *os.File
	bw       *bufio.Writer
	br       *bufio.Reader
	sid      int64 //segment id
	offset   int64 //segment current offset
	byteSize int32 //segment size
	chunks   []*Chunk
	isOpen   int32
}

func (self *Segment) Open() error {

	if atomic.CompareAndSwapInt32(&self.isOpen, 0, 1) {

		rf, err := os.OpenFile(self.path+string(filepath.Separator)+self.name,
			os.O_CREATE|os.O_RDWR, os.ModePerm)
		if nil != err {
			log.Error("MemorySnapshot|Load Segments|Open|FAIL|%s|%s\n", err, self.name)
			return err
		}

		wf, err := os.OpenFile(self.path+string(filepath.Separator)+self.name,
			os.O_CREATE|os.O_RDWR|os.O_APPEND, os.ModePerm)
		if nil != err {
			log.Error("MemorySnapshot|Load Segments|Open|FAIL|%s|%s\n", err, self.name)
			return err
		}
		self.rf = rf
		self.wf = wf

		//buffer
		self.br = bufio.NewReader(rf)
		self.bw = bufio.NewWriter(wf)

		//load
		self.loadCheck()

		log.Info("Segment|Open|SUCC|%s\n", self.name)
		return nil
	}
	return nil
}

//load check
func (self *Segment) loadCheck() {

	header := make([]byte, CHUNK_HEADER)
	chunkId := int64(0)
	fi, _ := self.rf.Stat()
	offset := int64(0)
	byteSize := int32(0)
	for {

		hl, err := io.ReadFull(self.br, header)
		if nil != err {
			if io.EOF != err {
				log.Error("Segment|Load Segment|Read Header|FAIL|%s|%s\n", err, self.name)
				continue
			}
			break
		}

		if hl <= 0 || hl < CHUNK_HEADER {
			log.Error("Segment|Load Segment|Read Header|FAIL|%s|%d\n", self.name, hl)
			break
		}

		//length
		length := binary.BigEndian.Uint32(header[0:4])

		al := offset + int64(length)
		//checklength
		if al > fi.Size() {
			log.Error("Segment|Load Segment|FILE SIZE|%s|%d/%d|offset:%d|length:%d\n", self.name, al, fi.Size(), self.offset, length)
			break
		}

		//checksum
		checksum := binary.BigEndian.Uint32(header[4:8])

		//read data
		l := length - CHUNK_HEADER
		data := make([]byte, l)
		dl, err := io.ReadFull(self.br, data)
		if nil != err || dl < int(l) {
			log.Error("Segment|Load Segment|Read Data|FAIL|%s|%s|%d/%d\n", err, self.name, l, dl)
			break
		}

		csum := crc32.ChecksumIEEE(data)
		//checkdata
		if csum != checksum {
			log.Error("Segment|Load Segment|Data Checksum|FAIL|%s|%d/%d\n", self.name, csum, checksum)
			break
		}

		offset += int64(length)

		//read chunkid
		chunkId = int64(binary.BigEndian.Uint64(header[8:16]))

		//flag
		flag := header[16]

		//add byteSize
		byteSize += int32(length)

		//create chunk
		chunk := &Chunk{
			offset:   offset,
			length:   int32(length),
			checksum: checksum,
			id:       chunkId,
			flag:     ChunkFlag(flag),
			data:     data}

		self.chunks = append(self.chunks, chunk)

	}

	self.offset = offset
	self.byteSize = byteSize

}

func (self *Segment) Delete(cid int64) {
	idx := int(cid - self.sid)
	// log.Debug("Segment|Delete|chunkid:%d|%s\n", cid, idx)
	if idx < len(self.chunks) {
		//mark delete
		s := self.chunks[idx]
		// log.Debug("Segment|Delete|%s", s)
		if s.flag != DELETE {
			s.flag = DELETE
			//flush to file
			self.wf.WriteAt([]byte{byte(DELETE)}, CHUNK_HEADER-1)
		}
	}
}

//get chunk by chunkid
func (self *Segment) Get(cid int64) *Chunk {
	// log.Debug("Segment|Get|%d\n", len(self.chunks))
	idx := sort.Search(len(self.chunks), func(i int) bool {
		// log.Debug("Segment|Get|%d|%d\n", i, cid)
		return self.chunks[i].id >= cid
	})

	// log.Debug("Segment|Get|Result|%d|%d|%d\n", idx, cid, len(self.chunks))
	//not exsit
	if idx >= len(self.chunks) {
		return nil
	} else if self.chunks[idx].id == cid {
		//delete data return nil
		if self.chunks[idx].flag == DELETE {
			return nil
		}
		return self.chunks[idx]
	} else {
		return nil
	}
}

//apend data
func (self *Segment) Append(chunks []*Chunk) error {

	buff := make([]byte, 0, 2*1024)
	for _, c := range chunks {
		buff = append(buff, c.marshal()...)
	}

	l, err := self.bw.Write(buff)
	if nil != err || l != len(buff) {
		log.Error("Segment|Append|FAIL|%s|%d/%d\n", err, l, len(buff))
		return err
	}
	self.bw.Flush()

	//tmp cache chunk
	if nil == self.chunks {
		self.chunks = make([]*Chunk, 0, 1000)
	}
	self.chunks = append(self.chunks, chunks...)

	//move offset
	self.offset += int64(len(buff))
	self.byteSize += int32(len(buff))
	return nil
}

func (self *Segment) Close() error {

	if atomic.CompareAndSwapInt32(&self.isOpen, 1, 0) {

		err := self.bw.Flush()
		if nil != err {
			log.Error("Segment|Close|Writer|FLUSH|FAIL|%s|%s|%s\n", err, self.path, self.name)
		}

		//free chunk memory
		self.chunks = nil

		err = self.wf.Close()
		if nil != err {
			log.Error("Segment|Close|Write FD|FAIL|%s|%s|%s\n", err, self.path, self.name)
			return err
		} else {
			err = self.rf.Close()
			if nil != err {
				log.Error("Segment|Close|Read FD|FAIL|%s|%s|%s\n", err, self.path, self.name)
			}
			return err
		}

	} else {
		return self.Close()
	}

}

//----------------------------------------------------
//|length 4byte|checksum 4byte|id 8byte|flag 1byte| data variant|
//----------------------------------------------------
//存储块
type Chunk struct {
	offset   int64
	length   int32
	checksum uint32
	flag     ChunkFlag //chunk状态
	id       int64
	data     []byte // data
}

func (self *Chunk) marshal() []byte {

	buff := make([]byte, self.length)
	//encode
	binary.BigEndian.PutUint32(buff[0:4], uint32(self.length))
	binary.BigEndian.PutUint32(buff[4:8], self.checksum)
	binary.BigEndian.PutUint64(buff[8:16], uint64(self.id))
	buff[16] = uint8(self.flag)
	copy(buff[17:], self.data)

	return buff
}

type Segments []*Segment

func (self Segments) Len() int { return len(self) }
func (self Segments) Swap(i, j int) {
	self[i], self[j] = self[j], self[i]
}
func (self Segments) Less(i, j int) bool {
	return self[i].sid < self[j].sid
}