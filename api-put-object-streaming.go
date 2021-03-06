/*
 * Minio Go Library for Amazon S3 Compatible Cloud Storage (C) 2017 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package minio

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sort"
	"strings"

	"github.com/minio/minio-go/pkg/credentials"
	"github.com/minio/minio-go/pkg/s3utils"
)

// PutObjectStreaming using AWS streaming signature V4
func (c Client) PutObjectStreaming(bucketName, objectName string, reader io.Reader) (n int64, err error) {
	return c.PutObjectWithProgress(bucketName, objectName, reader, nil, nil)
}

// putObjectMultipartStream - upload a large object using
// multipart upload and streaming signature for signing payload.
// Comprehensive put object operation involving multipart uploads.
//
// Following code handles these types of readers.
//
//  - *os.File
//  - *minio.Object
//  - Any reader which has a method 'ReadAt()'
//
func (c Client) putObjectMultipartStream(bucketName, objectName string,
	reader io.Reader, size int64, metadata map[string][]string, progress io.Reader) (n int64, err error) {

	// Set streaming signature.
	c.overrideSignerType = credentials.SignatureV4Streaming

	// Verify if reader is *minio.Object, *os.File or io.ReaderAt.
	// NOTE: Verification of object is kept for a specific purpose
	// while it is going to be duck typed similar to io.ReaderAt.
	// It is to indicate that *minio.Object implements io.ReaderAt.
	// and such a functionality is used in the subsequent code path.
	if isObject(reader) || isReadAt(reader) || isFile(reader) {
		n, err = c.putObjectMultipartStreamFromReadAt(bucketName, objectName, reader.(io.ReaderAt), size, metadata, progress)
	} else {
		n, err = c.putObjectMultipartStreamNoChecksum(bucketName, objectName, reader, size, metadata, progress)
	}
	if err != nil {
		errResp := ToErrorResponse(err)
		// Verify if multipart functionality is not available, if not
		// fall back to single PutObject operation.
		if errResp.Code == "AccessDenied" && strings.Contains(errResp.Message, "Access Denied") {
			// Verify if size of reader is greater than '5GiB'.
			if size > maxSinglePutObjectSize {
				return 0, ErrEntityTooLarge(size, maxSinglePutObjectSize, bucketName, objectName)
			}
			// Fall back to uploading as single PutObject operation.
			return c.putObjectNoChecksum(bucketName, objectName, reader, size, metadata, progress)
		}
	}
	return n, err
}

// uploadedPartRes - the response received from a part upload.
type uploadedPartRes struct {
	Error   error // Any error encountered while uploading the part.
	PartNum int   // Number of the part uploaded.
	Size    int64 // Size of the part uploaded.
	Part    *ObjectPart
}

type uploadPartReq struct {
	PartNum int         // Number of the part uploaded.
	Part    *ObjectPart // Size of the part uploaded.
}

// putObjectMultipartFromReadAt - Uploads files bigger than 64MiB.
// Supports all readers which implements io.ReaderAt interface
// (ReadAt method).
//
// NOTE: This function is meant to be used for all readers which
// implement io.ReaderAt which allows us for resuming multipart
// uploads but reading at an offset, which would avoid re-read the
// data which was already uploaded. Internally this function uses
// temporary files for staging all the data, these temporary files are
// cleaned automatically when the caller i.e http client closes the
// stream after uploading all the contents successfully.
func (c Client) putObjectMultipartStreamFromReadAt(bucketName, objectName string,
	reader io.ReaderAt, size int64, metadata map[string][]string, progress io.Reader) (int64, error) {
	// Input validation.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return 0, err
	}
	if err := s3utils.CheckValidObjectName(objectName); err != nil {
		return 0, err
	}

	// Initiate a new multipart upload.
	uploadID, err := c.newUploadID(bucketName, objectName, metadata)
	if err != nil {
		return 0, err
	}

	// Total data read and written to server. should be equal to 'size' at the end of the call.
	var totalUploadedSize int64

	// Complete multipart upload.
	var complMultipartUpload completeMultipartUpload

	// Calculate the optimal parts info for a given size.
	totalPartsCount, partSize, lastPartSize, err := optimalPartInfo(size)
	if err != nil {
		return 0, err
	}

	// Declare a channel that sends the next part number to be uploaded.
	// Buffered to 10000 because thats the maximum number of parts allowed
	// by S3.
	uploadPartsCh := make(chan uploadPartReq, 10000)

	// Declare a channel that sends back the response of a part upload.
	// Buffered to 10000 because thats the maximum number of parts allowed
	// by S3.
	uploadedPartsCh := make(chan uploadedPartRes, 10000)

	// Used for readability, lastPartNumber is always totalPartsCount.
	lastPartNumber := totalPartsCount

	// Send each part number to the channel to be processed.
	for p := 1; p <= totalPartsCount; p++ {
		uploadPartsCh <- uploadPartReq{PartNum: p, Part: nil}
	}
	close(uploadPartsCh)

	// Receive each part number from the channel allowing three parallel uploads.
	for w := 1; w <= totalWorkers; w++ {
		go func() {
			// Each worker will draw from the part channel and upload in parallel.
			for uploadReq := range uploadPartsCh {

				// If partNumber was not uploaded we calculate the missing
				// part offset and size. For all other part numbers we
				// calculate offset based on multiples of partSize.
				readOffset := int64(uploadReq.PartNum-1) * partSize

				// As a special case if partNumber is lastPartNumber, we
				// calculate the offset based on the last part size.
				if uploadReq.PartNum == lastPartNumber {
					readOffset = (size - lastPartSize)
					partSize = lastPartSize
				}

				// Get a section reader on a particular offset.
				sectionReader := io.NewSectionReader(reader, readOffset, partSize)

				// Proceed to upload the part.
				var objPart ObjectPart
				objPart, err = c.uploadPart(bucketName, objectName, uploadID, sectionReader,
					uploadReq.PartNum, nil, nil, partSize)
				if err != nil {
					uploadedPartsCh <- uploadedPartRes{
						Size:  0,
						Error: err,
					}
					// Exit the goroutine.
					return
				}

				// Save successfully uploaded part metadata.
				uploadReq.Part = &objPart

				// Send successful part info through the channel.
				uploadedPartsCh <- uploadedPartRes{
					Size:    objPart.Size,
					PartNum: uploadReq.PartNum,
					Part:    uploadReq.Part,
					Error:   nil,
				}
			}
		}()
	}

	// Gather the responses as they occur and update any
	// progress bar.
	for u := 1; u <= totalPartsCount; u++ {
		uploadRes := <-uploadedPartsCh
		if uploadRes.Error != nil {
			return totalUploadedSize, uploadRes.Error
		}
		// Retrieve each uploaded part and store it to be completed.
		// part, ok := partsInfo[uploadRes.PartNum]
		part := uploadRes.Part
		if part == nil {
			return 0, ErrInvalidArgument(fmt.Sprintf("Missing part number %d", uploadRes.PartNum))
		}
		// Update the totalUploadedSize.
		totalUploadedSize += uploadRes.Size
		// Update the progress bar if there is one.
		if progress != nil {
			if _, err = io.CopyN(ioutil.Discard, progress, uploadRes.Size); err != nil {
				return totalUploadedSize, err
			}
		}
		// Store the parts to be completed in order.
		complMultipartUpload.Parts = append(complMultipartUpload.Parts, CompletePart{
			ETag:       part.ETag,
			PartNumber: part.PartNumber,
		})
	}

	// Verify if we uploaded all the data.
	if totalUploadedSize != size {
		return totalUploadedSize, ErrUnexpectedEOF(totalUploadedSize, size, bucketName, objectName)
	}

	// Sort all completed parts.
	sort.Sort(completedParts(complMultipartUpload.Parts))
	_, err = c.completeMultipartUpload(bucketName, objectName, uploadID, complMultipartUpload)
	if err != nil {
		return totalUploadedSize, err
	}

	// Return final size.
	return totalUploadedSize, nil
}

func (c Client) putObjectMultipartStreamNoChecksum(bucketName, objectName string,
	reader io.Reader, size int64, metadata map[string][]string, progress io.Reader) (int64, error) {
	// Input validation.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return 0, err
	}
	if err := s3utils.CheckValidObjectName(objectName); err != nil {
		return 0, err
	}

	// Initiates a new multipart request
	uploadID, err := c.newUploadID(bucketName, objectName, metadata)
	if err != nil {
		return 0, err
	}

	// Calculate the optimal parts info for a given size.
	totalPartsCount, partSize, lastPartSize, err := optimalPartInfo(size)
	if err != nil {
		return 0, err
	}

	// Total data read and written to server. should be equal to 'size' at the end of the call.
	var totalUploadedSize int64

	// Initialize parts uploaded map.
	partsInfo := make(map[int]ObjectPart)

	// Part number always starts with '1'.
	var partNumber int
	for partNumber = 1; partNumber <= totalPartsCount; partNumber++ {
		// Update progress reader appropriately to the latest offset
		// as we read from the source.
		hookReader := newHook(reader, progress)

		// Proceed to upload the part.
		if partNumber == totalPartsCount {
			partSize = lastPartSize
		}

		var objPart ObjectPart
		objPart, err = c.uploadPart(bucketName, objectName, uploadID,
			io.LimitReader(hookReader, partSize), partNumber, nil, nil, partSize)
		// For unknown size, Read EOF we break away.
		// We do not have to upload till totalPartsCount.
		if err == io.EOF && size < 0 {
			break
		}

		if err != nil {
			return totalUploadedSize, err
		}

		// Save successfully uploaded part metadata.
		partsInfo[partNumber] = objPart

		// Save successfully uploaded size.
		totalUploadedSize += partSize
	}

	// Verify if we uploaded all the data.
	if size > 0 {
		if totalUploadedSize != size {
			return totalUploadedSize, ErrUnexpectedEOF(totalUploadedSize, size, bucketName, objectName)
		}
	}

	// Complete multipart upload.
	var complMultipartUpload completeMultipartUpload

	// Loop over total uploaded parts to save them in
	// Parts array before completing the multipart request.
	for i := 1; i < partNumber; i++ {
		part, ok := partsInfo[i]
		if !ok {
			return 0, ErrInvalidArgument(fmt.Sprintf("Missing part number %d", i))
		}
		complMultipartUpload.Parts = append(complMultipartUpload.Parts, CompletePart{
			ETag:       part.ETag,
			PartNumber: part.PartNumber,
		})
	}

	// Sort all completed parts.
	sort.Sort(completedParts(complMultipartUpload.Parts))
	_, err = c.completeMultipartUpload(bucketName, objectName, uploadID, complMultipartUpload)
	if err != nil {
		return totalUploadedSize, err
	}

	// Return final size.
	return totalUploadedSize, nil
}

// putObjectNoChecksum special function used Google Cloud Storage. This special function
// is used for Google Cloud Storage since Google's multipart API is not S3 compatible.
func (c Client) putObjectNoChecksum(bucketName, objectName string, reader io.Reader, size int64, metaData map[string][]string, progress io.Reader) (n int64, err error) {
	// Input validation.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return 0, err
	}
	if err := s3utils.CheckValidObjectName(objectName); err != nil {
		return 0, err
	}

	// Size -1 is only supported on Google Cloud Storage, we error
	// out in all other situations.
	if size < 0 && !s3utils.IsGoogleEndpoint(c.endpointURL) {
		return 0, ErrEntityTooSmall(size, bucketName, objectName)
	}
	if size > 0 {
		readerAt, ok := reader.(io.ReaderAt)
		if ok {
			reader = io.NewSectionReader(readerAt, 0, size)
		}
	}

	// Update progress reader appropriately to the latest offset as we
	// read from the source.
	readSeeker := newHook(reader, progress)

	// This function does not calculate sha256 and md5sum for payload.
	// Execute put object.
	st, err := c.putObjectDo(bucketName, objectName, readSeeker, nil, nil, size, metaData)
	if err != nil {
		return 0, err
	}
	if st.Size != size {
		return 0, ErrUnexpectedEOF(st.Size, size, bucketName, objectName)
	}
	return size, nil
}

// putObjectDo - executes the put object http operation.
// NOTE: You must have WRITE permissions on a bucket to add an object to it.
func (c Client) putObjectDo(bucketName, objectName string, reader io.Reader, md5Sum []byte, sha256Sum []byte, size int64, metaData map[string][]string) (ObjectInfo, error) {
	// Input validation.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return ObjectInfo{}, err
	}
	if err := s3utils.CheckValidObjectName(objectName); err != nil {
		return ObjectInfo{}, err
	}

	// Set headers.
	customHeader := make(http.Header)

	// Set metadata to headers
	for k, v := range metaData {
		if len(v) > 0 {
			customHeader.Set(k, v[0])
		}
	}

	// If Content-Type is not provided, set the default application/octet-stream one
	if v, ok := metaData["Content-Type"]; !ok || len(v) == 0 {
		customHeader.Set("Content-Type", "application/octet-stream")
	}

	// Populate request metadata.
	reqMetadata := requestMetadata{
		bucketName:         bucketName,
		objectName:         objectName,
		customHeader:       customHeader,
		contentBody:        reader,
		contentLength:      size,
		contentMD5Bytes:    md5Sum,
		contentSHA256Bytes: sha256Sum,
	}

	// Execute PUT an objectName.
	resp, err := c.executeMethod("PUT", reqMetadata)
	defer closeResponse(resp)
	if err != nil {
		return ObjectInfo{}, err
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			return ObjectInfo{}, httpRespToErrorResponse(resp, bucketName, objectName)
		}
	}

	var objInfo ObjectInfo
	// Trim off the odd double quotes from ETag in the beginning and end.
	objInfo.ETag = strings.TrimPrefix(resp.Header.Get("ETag"), "\"")
	objInfo.ETag = strings.TrimSuffix(objInfo.ETag, "\"")
	// A success here means data was written to server successfully.
	objInfo.Size = size

	// Return here.
	return objInfo, nil
}
