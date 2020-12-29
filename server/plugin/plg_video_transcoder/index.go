package plg_video_transcoder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	. "github.com/mickael-kerjean/filestash/server/common"
	. "github.com/mickael-kerjean/filestash/server/middleware"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	HLS_SEGMENT_LENGTH = 10
	CLEAR_CACHE_AFTER = 12
	VideoCachePath = "data/cache/video/"
)

func init(){
	ffmpegIsInstalled := false
	ffprobeIsInstalled := false
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		ffmpegIsInstalled = true
	}
	if _, err := exec.LookPath("ffprobe"); err == nil {
		ffprobeIsInstalled = true
	}
	plugin_enable := func() bool {
		return Config.Get("features.video.enable_transcoder").Schema(func(f *FormElement) *FormElement {
			if f == nil {
				f = &FormElement{}
			}
			f.Name = "enable_transcoder"
			f.Type = "enable"
			f.Target = []string{"transcoding_blacklist_format"}
			f.Description = "Enable/Disable on demand video transcoding. The transcoder"
			f.Default = true
			if ffmpegIsInstalled == false || ffprobeIsInstalled == false {
				f.Default = false
			}
			return f
		}).Bool()
	}

	blacklist_format := func() string {
		return Config.Get("features.video.blacklist_format").Schema(func(f *FormElement) *FormElement {
			if f == nil {
				f = &FormElement{}
			}
			f.Id = "transcoding_blacklist_format"
			f.Name = "blacklist_format"
			f.Type = "text"
			f.Description = "Video format that won't be transcoded"
			f.Default = os.Getenv("FEATURE_TRANSCODING_VIDEO_BLACKLIST")
			if f.Default != "" {
				f.Placeholder = fmt.Sprintf("Default: '%s'", f.Default)
			}
			return f
		}).String()
	}
	blacklist_format()

	if plugin_enable() == false {
		return
	} else if ffmpegIsInstalled == false {
		Log.Warning("[plugin video transcoder] ffmpeg needs to be installed")
		return
	} else if ffprobeIsInstalled == false {
		Log.Warning("[plugin video transcoder] ffprobe needs to be installed")
		return
	}

	cachePath := filepath.Join(GetCurrentDir(), VideoCachePath)
	os.MkdirAll(cachePath, os.ModePerm)

	Hooks.Register.ProcessFileContentBeforeSend(hls_playlist)
	Hooks.Register.HttpEndpoint(func(r *mux.Router, app *App) error {
		r.PathPrefix("/hls").Handler(NewMiddlewareChain(
			get_transcoded_segment,
			[]Middleware{ SecureHeaders },
			*app,
		)).Methods("GET")
		return nil
	})

	Hooks.Register.HttpEndpoint(func(r *mux.Router, app *App) error {
		r.HandleFunc(OverrideVideoSourceMapper, func(res http.ResponseWriter, req *http.Request) {
			res.Header().Set("Content-Type", GetMimeType(req.URL.String()))
			res.Write([]byte(`window.overrides["video-map-sources"] = function(sources){`))
			res.Write([]byte(`    return sources.map(function(source){`))

			blacklists := strings.Split(blacklist_format(), ",")
			for i:=0; i<len(blacklists); i++ {
				blacklists[i] = strings.TrimSpace(blacklists[i])
				res.Write([]byte(fmt.Sprintf(`if(source.type == "%s"){ return source; } `, GetMimeType("." + blacklists[i]))))
			}
			res.Write([]byte(`        source.src = source.src + "&transcode=hls";`))
			res.Write([]byte(`        source.type = "application/x-mpegURL";`))
			res.Write([]byte(`        return source;`))
			res.Write([]byte(`    })`))
			res.Write([]byte(`}`))
		})
		return nil
	})
}

func hls_playlist(reader io.ReadCloser, ctx *App, res *http.ResponseWriter, req *http.Request) (io.ReadCloser, error) {
	query := req.URL.Query()
	if query.Get("transcode") != "hls" {
		return reader, nil
	}
	path := query.Get("path")
	if strings.HasPrefix(GetMimeType(path), "video/") == false {
		return reader, nil
	}

	cacheName := "vid_" + GenerateID(ctx) + "_" + QuickHash(path, 10) + ".dat"
	cachePath := filepath.Join(
		GetCurrentDir(),
		VideoCachePath,
		cacheName,
	)

	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
	    f, err := os.OpenFile(cachePath, os.O_CREATE | os.O_RDWR, os.ModePerm)
	    if err != nil {
		    Log.Stdout("ERR %+v", err)
		    return reader, err
	    }
	    io.Copy(f, reader)
	    reader.Close()
	    f.Close()
	} else {
	    reader.Close()
	}

	cachePathFolder := cachePath + "_transcoded"
	if _, err := os.Stat(cachePathFolder); os.IsNotExist(err) {
		os.MkdirAll(cachePathFolder, os.ModePerm)
		go hls_transcode(cachePath, cacheName)
	}

	var response string
	playlistPath := cachePath + ".m3u8"
	if _, err := os.Stat(playlistPath); err == nil {
		content, err := ioutil.ReadFile(playlistPath)
		if err != nil {
			Log.Info("[plugin hls]: couldn't read playlist file %+v", err)
			return reader, err
		}

		response = string(content)
	} else {
		response =  "#EXTM3U\n"
		response += "#EXT-X-VERSION:3\n"
		response += "#EXT-X-MEDIA-SEQUENCE:0\n"
		response += "#EXT-X-ALLOW-CACHE:YES\n"
	}

	(*res).Header().Set("Content-Type", "application/x-mpegURL")
	return NewReadCloserFromBytes([]byte(response)), nil
}

func hls_transcode(cachePath string, cacheName string) {
	cachePathFolder := cachePath + "_transcoded"

	cmd := exec.Command("ffmpeg", []string{
		"-i", cachePath,
		"-vf", fmt.Sprintf("scale=-2:%d", 720),
		"-vcodec", "libx264",
		"-preset", "veryfast",
		"-acodec", "libfdk_aac",
		"-vbr", "5",
		"-pix_fmt", "yuv420p",
		"-x264opts:0", "subme=0:me_range=4:rc_lookahead=10:me=dia:no_chroma_me:8x8dct=0:partitions=none",
		"-f", "hls",
		"-hls_playlist_type", "event",
		"-hls_base_url", fmt.Sprintf("/hls?path=%s&file=", cacheName),
		"-hls_time", fmt.Sprintf("%d.00", HLS_SEGMENT_LENGTH),
		"-hls_segment_filename", cachePathFolder + "/%03d.ts",
		"-copyts",
		"-vsync", "2",
		cachePath + ".m3u8",
	}...)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

func get_transcoded_segment(ctx App, res http.ResponseWriter, req *http.Request) {
	cachePath := filepath.Join(
		GetCurrentDir(),
		VideoCachePath,
		req.URL.Query().Get("path"),
	)

	transcodedSegmentPath := filepath.Join(
		cachePath + "_transcoded",
		req.URL.Query().Get("file"),
	)

	if _, err := os.Stat(transcodedSegmentPath); err == nil {
		f, err := os.Open(transcodedSegmentPath)
		if err != nil {
			Log.Info("[plugin hls]: couldn't read transcoded segment %+v", err)
			return
		}
		io.Copy(res, f)
		f.Close()
	}
}

type FFProbeData struct {
	Format struct {
		Duration float64 `json:"duration,string"`
		BitRate int `json:"bit_rate,string"`
	} `json: "format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		PixelFormat string `json:"pix_fmt"`
	} `json:"streams"`
}

func ffprobe(videoPath string) (FFProbeData, error) {
	var stream bytes.Buffer
	var probe FFProbeData

	cmd := exec.Command(
		"ffprobe", strings.Split(fmt.Sprintf(
			"-v quiet -print_format json -show_format -show_streams %s",
			videoPath,
		), " ")...
	)
	cmd.Stdout = &stream
	if err := cmd.Run(); err != nil {
		return probe, nil
	}
	cmd.Run()
	if err := json.Unmarshal([]byte(stream.String()), &probe); err != nil {
		return probe, err
	}
	return probe, nil
}
