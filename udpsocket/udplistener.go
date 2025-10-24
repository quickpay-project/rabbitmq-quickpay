package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io/ioutil"
	"net"
	"strings"
	"time"

	"gocv.io/x/gocv"
)

func main() {
	// UDP bağlantısı başlat
	conn, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 1234,
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer conn.Close()
	fmt.Println("UDP sunucusu başlatıldı. Bekleniyor...")
	// img := gocv.NewMat()
	window := gocv.NewWindow("Received Vide")
	sliceData := []byte{}
	buffer := make([]byte, 1024)
	for {
		// fmt.Println("Start:")
		// buffer := make([]byte, 1024)

		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Veri okunamadı:", err)
			fmt.Println(err)
			continue
		}
		sliceData = append(sliceData, deCompresse(buffer[:n])...)

		if hasEOF(deCompresse(buffer[:n])) {
			// Veri tamamlandı
			// fmt.Println("Boyut:", len(sliceData))
			receivedData2 := removeEOF(sliceData)

			// fmt.Println("SON Boyut:", len(receivedData2))
			sliceData = nil // receivedData'yi sıfırlayın
			// img, err := gocv.NewMatFromBytes(300, 300, 1, receivedData2)
			img, err := gocv.NewMatFromBytes(1080, 1920, gocv.MatTypeCV8UC3, receivedData2)
			// img, err = gocv.IMDecode(buffer[:n], gocv.IMReadUnchanged)
			if err != nil {
				fmt.Println("Error decoding image:", err)
				continue
			}
			time.Sleep(10 * time.Millisecond)
			window.IMShow(img)
			window.WaitKey(1)
		}
	}
}

func hasEOF(chunk []byte) bool {
	// Burada son parçayı tanımlamanız gerekebilir, örneğin "EOF" işareti ile
	return strings.Contains(string(chunk), "EOF")
}

func removeEOF(data []byte) []byte {
	// Verinin sonundaki "EOF" işaretini kaldırın
	return data[:len(data)-3]
}

func deCompresse(compressedBytes []byte) []byte {
	// Decompress the data
	reader, err := gzip.NewReader(bytes.NewBuffer(compressedBytes))
	if err != nil {
		fmt.Println("Error creating Gzip reader:", err)
	}
	decompressedData, err := ioutil.ReadAll(reader)
	if err != nil {
		fmt.Println("Error reading decompressed data:", err)
	}
	return decompressedData
}
