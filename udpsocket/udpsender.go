package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"log"
	"net"
	"time"

	"gocv.io/x/gocv"
)

func main() {
	// UDP soket oluştur
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 1234,
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer conn.Close()

	webcam, _ := gocv.OpenVideoCapture(0)
	window := gocv.NewWindow("Hello")
	img := gocv.NewMat()
	// bytData := []byte("Celal Sahin Altisik")
	for {

		if ok := webcam.Read(&img); !ok {
			fmt.Printf("cannot read device %v\n", 0)
			return
		}
		if img.Empty() {
			continue
		}
		time.Sleep(10 * time.Millisecond)
		window.IMShow(img)
		window.WaitKey(1)
		// fmt.Println("Compress Boyut:", cap(compresse(img.ToBytes())))
		sendLargeData(img.ToBytes(), conn)
		// fmt.Println("Boyut:", len(img.ToBytes()))

		// fmt.Println("1")

	}
}

func compresse(data []byte) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write(data); err != nil {
		log.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		log.Fatal(err)
	}
	// Convert compressed data to bytes
	return b.Bytes()
}

func sendLargeData(data []byte, conn *net.UDPConn) {
	packetSize := 1024 // Parça boyutunu ayarlayın

	for i := 0; i < len(data); i += packetSize {
		end := i + packetSize
		if end > len(data) {
			end = len(data)
		}

		chunk := data[i:end]
		_, err := conn.Write(compresse(chunk))
		if err != nil {
			// fmt.Println("Hata veri gönderirken:", len(chunk), err)
			return
		}
	}

	// Gönderimin sonunda EOF işareti gönder
	eof := []byte("EOF")
	_, err := conn.Write(compresse(eof))
	if err != nil {
		fmt.Println("Hata EOF gönderirken:", err)
		return
	}
	// fmt.Println("Boyutx:", len(append(data, eof...)))

}
