package winrmcp

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/masterzen/winrm"
	"github.com/nu7hatch/gouuid"
)

type uploadJob struct {
	uploadPath string
	chunk      []byte
	n          int
}

func doCopy(client *winrm.Client, config *Config, in io.Reader, toPath string) error {
	uniquePart, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("Error generating unique filename: %v", err)
	}

	tempFile := fmt.Sprintf("%%%%TEMP%%%%\\winrmcp-%s-%%d.tmp", uniquePart)
	//tempFile := fmt.Sprintf("$env:TEMP\\winrmcp-%s-%%d.tmp", uniquePart)
	//tempFile := fmt.Sprintf("C:\\Users\\vagrant\\winrmcp-%s-%%d", uniquePart)

	// Create a buffer to write our archive to.
	buf := new(bytes.Buffer)

	b64Enc := base64.NewEncoder(base64.StdEncoding, buf)

	// Create a new zip archive.
	w := zip.NewWriter(b64Enc)
	f, err := w.Create(toPath)
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, in); err != nil {
		return err
	}
	// Make sure to check the error on Close.
	if err := w.Close(); err != nil {
		return err
	}

	if err := b64Enc.Close(); err != nil {
		return err
	}

	jobs := make(chan *uploadJob, 2000)
	concurrentUploads := 30
	done := make(chan struct{})
	//defer close(done)
	var wg sync.WaitGroup

	wg.Add(concurrentUploads)
	for i := 0; i < concurrentUploads; i++ {
		log.Printf("starting [%d]", i)
		go func(wid int) {
			func() {
				for j := range jobs {
					log.Printf("worker[%d]: doin a job", wid)
					if err := retry(func() error {
						return writeChunk(client, j.uploadPath, string(j.chunk[:j.n]))
					}, 3); err != nil {
						log.Printf("error writing chunk: %s", err)
						go func() {
							done <- struct{}{}
						}()
					}
					select {
					case <-done:
						log.Println("worker is done")
						return
					default:
					}
				}
			}()
			wg.Done()
		}(i)
	}

	for i := 0; ; i++ {
		tempPathChunk := fmt.Sprintf(tempFile, i)
		cs := chunkSize(tempPathChunk)
		chunk := make([]byte, cs)
		n, err := buf.Read(chunk)

		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}

		log.Printf("making job %s. Uploading to %s", i, tempPathChunk)
		select {
		case jobs <- &uploadJob{tempPathChunk, chunk, n}:
		case <-done:
			return errors.New("upload cancelled")
		}
	}
	close(jobs)
	wg.Wait()

	coalesce(client, toPath)

	/*

		/*
			done := false
			for !done {
				done, err = uploadChunks(client, tempPath, config.MaxOperationsPerShell, buf)
				if err != nil {
					return fmt.Errorf("Error uploading file to %s: %v", tempPath, err)
				}
			}

			if os.Getenv("WINRMCP_DEBUG") != "" {
				log.Printf("Moving file from %s to %s", tempPath, toPath)
			}

			err = restoreContent(client, tempPath, toPath)
			if err != nil {
				return fmt.Errorf("Error restoring file from %s to %s: %v", tempPath, toPath, err)
			}

			if os.Getenv("WINRMCP_DEBUG") != "" {
				log.Printf("Removing temporary file %s", tempPath)
			}

			err = cleanupContent(client, tempPath)
			if err != nil {
				return fmt.Errorf("Error removing temporary file %s: %v", tempPath, err)
			}
	*/

	return nil
}

type RetryableFunc func() error

func retry(f RetryableFunc, maxTries int) error {
	var err error
	for i := 0; i < maxTries; i++ {
		log.Printf("try %d", i)
		err = f()
		if err != nil {
			log.Println(err.Error())
			continue
		}
		return nil
	}
	return fmt.Errorf("Retries exhausted: %s", err.Error())
}

func chunkSize(filePath string) int {
	// Upload the file in chunks to get around the Windows command line size limit.

	//return 8192 - len(filePath)
	return 7000 - len(filePath)
}

func writeChunk(client *winrm.Client, filePath, content string) error {
	// this one works with %TEMP% path
	//scmd := fmt.Sprintf(`powershell.exe -Command "& {Set-Content -NoNewLine -Value '%s' -Path '%s'}"`, content, filePath)
	scmd := fmt.Sprintf(`echo %s> "%s"`, content, filePath)

	log.Println(scmd)
	log.Printf("Appending content: (len=%d)", len(scmd))

	_, _, code, err := client.RunWithString(scmd, "")

	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("upload operation returned code=%d", code)
	}

	return nil
}

func coalesce(client *winrm.Client, toPath string) error {
	shell, err := client.CreateShell()
	if err != nil {
		return err
	}

	defer shell.Close()
	script := fmt.Sprintf(`Add-Type -AssemblyName System.IO.Compression.FileSystem

	Get-ChildItem $env:TEMP\winrmcp-*.tmp | Sort-Object {[int]$_.Name.Split("-.")[6]} | Get-Content -raw | foreach-object {$_ -replace "` + "`" + `n", ""} | Out-File -NoNewline $env:TEMP\combined.zip.b64
	$base64string = Get-Content -raw $env:TEMP\combined.zip.b64
	[IO.File]::WriteAllBytes("$env:TEMP\combined.zip", [Convert]::FromBase64String($base64string))
	[System.IO.Compression.ZipFile]::ExtractToDirectory("$env:TEMP\combined.zip", ".")
	`)

	a, b, code, err := client.RunWithString(winrm.Powershell(script), "")
	if err != nil {
		return err
	}
	fmt.Println(a, b)

	if code != 0 {
		return fmt.Errorf("restore operation returned code=%d", code)
	}
	return nil
}

func cleanupContent(client *winrm.Client, filePath string) error {
	shell, err := client.CreateShell()
	if err != nil {
		return err
	}

	defer shell.Close()
	script := fmt.Sprintf(`Remove-Item %s -ErrorAction SilentlyContinue`, filePath)

	cmd, err := shell.Execute(winrm.Powershell(script))
	if err != nil {
		return err
	}
	defer cmd.Close()

	var wg sync.WaitGroup
	copyFunc := func(w io.Writer, r io.Reader) {
		defer wg.Done()
		io.Copy(w, r)
	}

	wg.Add(2)
	go copyFunc(os.Stdout, cmd.Stdout)
	go copyFunc(os.Stderr, cmd.Stderr)

	cmd.Wait()
	wg.Wait()

	if cmd.ExitCode() != 0 {
		return fmt.Errorf("cleanup operation returned code=%d", cmd.ExitCode())
	}
	return nil
}
