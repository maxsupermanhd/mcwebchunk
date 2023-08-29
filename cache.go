/*
	WebChunk, web server for block game maps
	Copyright (C) 2022 Maxim Zhuchkov

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU Affero General Public License as published
	by the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU Affero General Public License for more details.

	You should have received a copy of the GNU Affero General Public License
	along with this program.  If not, see <https://www.gnu.org/licenses/>.

	Contact me via mail: q3.max.2011@yandex.ru or Discord: MaX#6717
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
)

var (
	imageCacheMaxCache     = 512
	imageCacheStorageLevel = 5
	imageCacheProcess      = make(chan imageTask, 32)
	powarr                 = []int{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096}
)

type cachedImage struct {
	img          *image.RGBA
	syncedToDisk bool
	lastUse      time.Time
}

type imageLoc struct {
	World, Dimension, Layer string
	S, X, Z                 int
}

func (i imageLoc) String() string {
	return fmt.Sprintf("{%s:%s:%s at %ds %dx %dz}", i.World, i.Dimension, i.Layer, i.S, i.X, i.Z)
}

type imageTask struct {
	loc imageLoc
	img *image.RGBA
	ret chan<- *image.RGBA
}

// Storing and caching only zoom level 5 (32x32), also aligns with the regions

func icAT(cx, cz int) (int, int) {
	return cx >> imageCacheStorageLevel, cz >> imageCacheStorageLevel
}

func icIN(cx, cz int) (int, int) {
	return cx & (powarr[imageCacheStorageLevel] - 1), cz & (powarr[imageCacheStorageLevel] - 1)
}

// Does actual IO
func imageCacheWorker(requests chan imageTask, responses chan imageTask) {
	for p := range requests {
		if p.img == nil { // read
			var err error
			p.img, err = cacheLoad(p.loc.World, p.loc.Dimension, p.loc.Layer, imageCacheStorageLevel, p.loc.X, p.loc.Z)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					log.Printf("Error reading image cache %v: %v", p.loc, err)
				}
				continue
			}
			responses <- p
		} else {
			err := cacheSave(p.img, p.loc.World, p.loc.Dimension, p.loc.Layer, imageCacheStorageLevel, p.loc.X, p.loc.Z)
			if err != nil {
				log.Printf("Error writing image cache %v: %v", p.loc, err)
			}
		}
	}
}

func imageCacheProcessor(ctx context.Context) {
	imageCache := map[imageLoc]cachedImage{}

	flushTicker := time.NewTicker(15 * time.Second)

	var wg sync.WaitGroup

	iotasks := make(chan imageTask, 32)
	ioreturn := make(chan imageTask, 128)

	ioreadwaiting := map[imageLoc][]chan<- *image.RGBA{}

	ion := cfg.GetDSInt(4, "cache_workers")
	wg.Add(ion)
	for i := 0; i < ion; i++ {
		go func() {
			imageCacheWorker(iotasks, ioreturn)
			wg.Done()
		}()
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Image cache sutting down...")
			savecount := 0
			for k, v := range imageCache {
				if !v.syncedToDisk {
					iotasks <- imageTask{
						loc: k,
						img: v.img,
						ret: nil,
					}
					savecount++
				}
			}
			log.Printf("Saving %d images...", savecount)
			close(iotasks)
			wg.Wait()
			log.Println("Image cache done")
			return
		case p := <-ioreturn:
			if p.img != nil { // was read
				i, ok := ioreadwaiting[p.loc]
				if !ok {
					log.Println("Useless read? ", p.loc)
					break
				}
				for _, r := range i {
					r <- p.img
				}
				delete(ioreadwaiting, p.loc)
			}
		case p := <-imageCacheProcess:
			if p.img == nil { // read
				if p.ret == nil {
					log.Printf("Requested image but no return channel?! %v", spew.Sdump(p))
					break
				}
				if p.loc.S == imageCacheStorageLevel {
					i, ok := imageCache[p.loc]
					if ok {
						p.ret <- copyImage(i.img)
						i.lastUse = time.Now()
						imageCache[p.loc] = i
						break
					}
					w, ok := ioreadwaiting[p.loc]
					if ok {
						w = append(w, p.ret)
						ioreadwaiting[p.loc] = w
						break
					}
					ioreadwaiting[p.loc] = []chan<- *image.RGBA{p.ret}
					iotasks <- p
				} else if p.loc.S < imageCacheStorageLevel {
					ax, az := p.loc.X*powarr[p.loc.S], p.loc.Z*powarr[p.loc.S]
					rx, rz := icAT(ax, az)
					ix, iz := icIN(ax, az)
					is := powarr[p.loc.S] * 16

					rl := imageLoc{World: p.loc.World, Dimension: p.loc.Dimension, Layer: p.loc.Layer, S: imageCacheStorageLevel, X: rx, Z: rz}
					i, ok := imageCache[rl]
					if !ok {
						var err error
						i.img, err = cacheLoad(p.loc.World, p.loc.Dimension, p.loc.Layer, imageCacheStorageLevel, rx, rz)
						if err != nil {
							if !errors.Is(err, os.ErrNotExist) {
								log.Printf("Weird stuff you got with image cache %v: %v", p.loc, err)
							}
							close(p.ret)
							break
						}
						i.syncedToDisk = true
						i.lastUse = time.Now()
						if len(imageCache) > imageCacheMaxCache {
							oldest := time.Now()
							oldestk := imageLoc{}
							for k, v := range imageCache {
								if oldest.After(v.lastUse) {
									oldest = v.lastUse
									oldestk = k
								}
							}
							delete(imageCache, oldestk)
						}
						imageCache[rl] = i
					} else {
						i.lastUse = time.Now()
						imageCache[rl] = i
					}

					ret := image.NewRGBA(image.Rect(0, 0, is, is))
					pt := image.Point{(ix / powarr[p.loc.S]) * is, (iz / powarr[p.loc.S]) * is}
					// log.Printf("Draw %4d %4d %4d %4d %#v", rl.x, rl.z, p.loc.x, p.loc.z, pt)
					draw.Draw(ret, ret.Rect, i.img, pt, draw.Over)

					p.ret <- ret

				} else { // p.loc.z > imageCacheStorageLevel
					// log.Printf("Unimlemented load of s > imageCacheStorageLevel, %#v", p.loc)
					if p.loc.S > 9 {
						// too big
						close(p.ret)
						break
					}
					ax, az := p.loc.X*powarr[p.loc.S], p.loc.Z*powarr[p.loc.S]
					bx, bz := icAT(ax, az)
					rs := powarr[p.loc.S-imageCacheStorageLevel]
					ret := image.NewRGBA(image.Rect(0, 0, rs*powarr[imageCacheStorageLevel]*16, rs*powarr[imageCacheStorageLevel]*16))

					for x := 0; x < rs; x++ {
						for z := 0; z < rs; z++ {
							rx, rz := bx+x, bz+z
							rl := imageLoc{World: p.loc.World, Dimension: p.loc.Dimension, Layer: p.loc.Layer, S: imageCacheStorageLevel, X: rx, Z: rz}
							i, ok := imageCache[rl]
							if !ok {
								var err error
								i.img, err = cacheLoad(p.loc.World, p.loc.Dimension, p.loc.Layer, imageCacheStorageLevel, rx, rz)
								if err != nil {
									if !errors.Is(err, os.ErrNotExist) {
										log.Printf("Weird stuff you got with image cache %v: %v", p.loc, err)
									}
									continue
								}
								i.syncedToDisk = true
								i.lastUse = time.Now()
								if len(imageCache) > imageCacheMaxCache {
									oldest := time.Now()
									oldestk := imageLoc{}
									for k, v := range imageCache {
										if oldest.After(v.lastUse) {
											oldest = v.lastUse
											oldestk = k
										}
									}
									delete(imageCache, oldestk)
								}
								imageCache[rl] = i
							}
							w := powarr[imageCacheStorageLevel] * 16
							// log.Printf("Draw %3d %3d %3d base %3d %3d tile %3d %3d to %3d %3d", p.loc.X, p.loc.Z, rs, bx, bz, rl.X, rl.Z, x*w, z*w)
							draw.Draw(ret, image.Rect(x*w, z*w, x*w+w, z*w+w), i.img, image.Point{}, draw.Src)
						}
					}
					p.ret <- ret
				}
			} else { // write
				if p.loc.S == imageCacheStorageLevel {
					iimg, ok := imageCache[p.loc]
					if !ok {
						imageCache[p.loc] = cachedImage{
							img:          p.img,
							syncedToDisk: false,
							lastUse:      time.Now(),
						}
						break
					}
					draw.Draw(iimg.img, iimg.img.Rect, p.img, image.Point{}, draw.Over)
					iimg.lastUse = time.Now()
					iimg.syncedToDisk = false
					break
				}
				if p.loc.S != 0 {
					// log.Printf("Unsupported cache write of scale %d", p.loc.s)
					break
				}
				rx, rz := icAT(p.loc.X, p.loc.Z)
				ix, iz := icIN(p.loc.X, p.loc.Z)

				rl := imageLoc{World: p.loc.World, Dimension: p.loc.Dimension, Layer: p.loc.Layer, S: imageCacheStorageLevel, X: rx, Z: rz}

				i, ok := imageCache[rl]
				if !ok {
					var err error
					i.img, err = cacheLoad(p.loc.World, p.loc.Dimension, p.loc.Layer, imageCacheStorageLevel, rx, rz)
					if err != nil {
						if !errors.Is(err, os.ErrNotExist) {
							log.Printf("Weird stuff you got with image cache %v: %v", p.loc, err)
							break
						} else {
							i.img = image.NewRGBA(image.Rect(0, 0, powarr[imageCacheStorageLevel]*16, powarr[imageCacheStorageLevel]*16))
						}
					}
					i.lastUse = time.Now()
					if len(imageCache) > imageCacheMaxCache {
						oldest := time.Now()
						oldestk := imageLoc{}
						for k, v := range imageCache {
							if oldest.After(v.lastUse) {
								oldest = v.lastUse
								oldestk = k
							}
						}
						delete(imageCache, oldestk)
					}
				}

				draw.Draw(i.img, image.Rect(ix*16, iz*16, ix*16+16, iz*16+16), p.img, image.Point{}, draw.Src)
				i.syncedToDisk = false

				imageCache[rl] = i
			}
		case <-flushTicker.C:
			saveCount := 0
			for k, v := range imageCache {
				if !v.syncedToDisk {
					if saveCount == 0 {
						log.Println("Saving images to disk...")
					}
					err := cacheSave(v.img, k.World, k.Dimension, k.Layer, k.S, k.X, k.Z)
					if err != nil {
						log.Printf("Failed to save cache of %s: %v", k, err)
						continue
					}
					v.syncedToDisk = true
					saveCount++
				}
			}
			if saveCount != 0 {
				log.Printf("Saved %d images to disk", saveCount)
			}
		}
	}
}

func copyImage(img *image.RGBA) *image.RGBA {
	ret := image.NewRGBA(img.Rect)
	copy(ret.Pix, img.Pix)
	return ret
}

func imageCacheGetBlocking(world, dim, render string, s, x, z int) *image.RGBA {
	return imageCacheGetBlockingLoc(imageLoc{world, dim, render, s, x, z})
}

func imageCacheGetBlockingLoc(loc imageLoc) *image.RGBA {
	recv := make(chan *image.RGBA)
	imageCacheProcess <- imageTask{
		loc: loc,
		img: nil,
		ret: recv,
	}
	for i := range recv {
		return i
	}
	return nil
}

func imageCacheSave(img *image.RGBA, world, dim, render string, s, x, z int) {
	imageCacheProcess <- imageTask{
		loc: imageLoc{
			World:     world,
			Dimension: dim,
			Layer:     render,
			S:         s,
			X:         x,
			Z:         z,
		},
		img: img,
		ret: nil,
	}
}

func getImageCachePrefix() string {
	prefix := os.Getenv("CACHE_PREFIX")
	if prefix == "" {
		prefix = "imageCache"
	}
	return prefix
}

func cacheGetFilename(world, dim, render string, s, x, z int) string {
	return path.Join(".", getImageCachePrefix(), world, dim, render, strconv.FormatInt(int64(s), 10), strconv.FormatInt(int64(x), 10)+"x"+strconv.FormatInt(int64(z), 10)+".png")
}

func cacheSave(img *image.RGBA, world, dim, render string, s, x, z int) error {
	storePath := cacheGetFilename(world, dim, render, s, x, z)
	err := os.MkdirAll(path.Dir(storePath), 0764)
	if err != nil {
		return err
	}
	file, err := os.Create(storePath)
	if err != nil {
		return err
	}
	err = png.Encode(file, img)
	if err != nil {
		return err
	}
	return file.Close()
}

func cacheLoad(world, dim, render string, s, x, z int) (*image.RGBA, error) {
	log.Printf("Image load [%s] [%s] [%s] %2d %3d %3d", world, dim, render, s, x, z)
	fp := cacheGetFilename(world, dim, render, s, x, z)
	f, err := os.Open(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ii, _, err := image.Decode(f)
	if err != nil {
		log.Printf("Failed to decode image cache %v: %v", fp, err)
		err = os.Remove(fp)
		if err != nil {
			log.Printf("Failed to remove broken cache file %v: %v", fp, err)
		}
		return nil, err
	}
	return imageToRGBA(ii), nil
}

func imageToRGBA(src image.Image) *image.RGBA {
	if dst, ok := src.(*image.RGBA); ok {
		return dst
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

func DirCountSize(path string) (count, size int64, err error) {
	err = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			count++
			size += info.Size()
		}
		return err
	})
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	return count, size, err
}

func getImageCacheCountSize(world, dim string) (int64, int64, error) {
	return DirCountSize(path.Join(".", getImageCachePrefix(), world, dim))
}
