package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"time"
	"vlog"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	demo()
}

func demo() {

	v, err := vlog.Open(vlog.Options{
		DataDir:    "./ndata",
		MaxLogSize: 64 * 1024 * 1024,
	})
	if err != nil {
		log.Println(err)
		return
	}

	defer v.Close()

	go func() {
		r, err := v.NewReader()
		if err != nil {
			log.Println(err)
			return
		}
		defer r.Close()

		count := 0
		for {
			e, err := r.Read()
			if err != nil {
				if errors.Is(err, vlog.ErrBlocked) {
					time.Sleep(time.Second)
					continue
				}

				log.Println(err)
				break
			}

			log.Printf("--- %v %v", string(e.Key), len(e.Data))

			time.Sleep(time.Millisecond * 3)

			count++
			if count%100 == 0 {
				_ = r.UpdateCheckpoint()
				_ = r.DeleteConsumedFiles()
			}
		}
	}()

	data := bytes.Repeat([]byte{'a'}, 1024*1024)
	for {
		// 9223372036854775807
		key := fmt.Sprintf("%020v", time.Now().UnixNano())
		_ = v.Append([]byte(key), data)

		time.Sleep(time.Millisecond * 100)
	}
}
