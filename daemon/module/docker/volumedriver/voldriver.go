package volumedriver

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/akutz/goof"
	"github.com/akutz/gotil"

	"github.com/emccode/rexray/core"
	"github.com/emccode/rexray/daemon/module"
)

const (
	modName = "docker"
)

type mod struct {
	r    *core.RexRay
	name string
	addr string
	desc string
}

var (
	separators  = regexp.MustCompile(`[ &_=+:]`)
	dashes      = regexp.MustCompile(`[\-]+`)
	illegalPath = regexp.MustCompile(`[^[:alnum:]\~\-\./]`)
)

func init() {
	module.RegisterModule(modName, newModule)
}

func newModule(c *module.Config) (module.Module, error) {

	host := strings.Trim(c.Address, " ")

	if host == "" {
		if c.Name == "default-docker" {
			host = "unix:///run/docker/plugins/rexray.sock"
		} else {
			fname := cleanName(c.Name)
			host = fmt.Sprintf("unix:///run/docker/plugins/%s.sock", fname)
		}
	}

	c.Address = host

	cc, err := c.Config.Copy()
	if err != nil {
		return nil, err
	}

	if !cc.GetBool("rexray.volume.path.disableCache") {
		cc.Set("rexray.volume.path.cache", true)
	}

	r := core.New(cc)
	r.Context = c.Name

	return &mod{
		r:    r,
		name: c.Name,
		desc: c.Description,
		addr: host,
	}, nil
}

func cleanName(s string) string {
	s = strings.Trim(strings.ToLower(s), " ")
	s = separators.ReplaceAllString(s, "-")
	s = illegalPath.ReplaceAllString(s, "")
	s = dashes.ReplaceAllString(s, "-")
	return s
}

const driverName = "docker"

var (
	errMissingHost      = goof.New("Missing host parameter")
	errBadHostSpecified = goof.New("Bad host specified, ie. unix:///run/docker/plugins/rexray.sock or tcp://127.0.0.1:8080")
	errBadProtocol      = goof.New("Bad protocol specified with host, ie. unix:// or tcp://")
)

type pluginRequest struct {
	Name string          `json:"Name,omitempty"`
	Opts core.VolumeOpts `json:"Opts,omitempty"`
}

func (m *mod) Start() error {

	proto, addr, parseAddrErr := gotil.ParseAddress(m.Address())
	if parseAddrErr != nil {
		return parseAddrErr
	}

	if proto == "unix" {
		dir := filepath.Dir(addr)
		os.MkdirAll(dir, 0755)
	}

	const validProtoPatt = "(?i)^unix|tcp$"
	isProtoValid, matchProtoErr := regexp.MatchString(validProtoPatt, proto)
	if matchProtoErr != nil {
		return goof.WithFieldsE(goof.Fields{
			"protocol":       proto,
			"validProtoPatt": validProtoPatt,
		}, "error matching protocol", matchProtoErr)
	}
	if !isProtoValid {
		return goof.WithField("protocol", proto, "invalid protocol")
	}

	if err := m.r.InitDrivers(); err != nil {
		return goof.WithFieldsE(goof.Fields{
			"m":   m,
			"m.r": m.r,
		}, "error initializing drivers", err)
	}

	if err := os.MkdirAll("/etc/docker/plugins", 0755); err != nil {
		return err
	}

	var specPath string
	var startFunc func() error

	mux := m.buildMux()

	if proto == "unix" {
		sockFile := addr
		sockFileDir := filepath.Dir(sockFile)
		mkSockFileDirErr := os.MkdirAll(sockFileDir, 0755)
		if mkSockFileDirErr != nil {
			return mkSockFileDirErr
		}

		_ = os.RemoveAll(sockFile)

		specPath = m.Address()
		startFunc = func() error {
			l, lErr := net.Listen("unix", sockFile)
			if lErr != nil {
				return lErr
			}
			defer l.Close()
			defer os.Remove(sockFile)

			return http.Serve(l, mux)
		}
	} else {
		specPath = addr
		startFunc = func() error {
			s := &http.Server{
				Addr:           addr,
				Handler:        mux,
				ReadTimeout:    10 * time.Second,
				WriteTimeout:   10 * time.Second,
				MaxHeaderBytes: 1 << 20,
			}
			return s.ListenAndServe()
		}
	}

	go func() {
		sErr := startFunc()
		if sErr != nil {
			panic(sErr)
		}
	}()

	spec := m.r.Config.GetString("spec")
	if spec == "" {
		if m.name == "default-docker" {
			spec = "/etc/docker/plugins/rexray.spec"
		} else {
			fname := cleanName(m.name)
			spec = fmt.Sprintf("/etc/docker/plugins/%s.spec", fname)
		}
	}

	log.WithField("path", spec).Debug("docker voldriver spec file")

	if !gotil.FileExists(spec) {
		if err := ioutil.WriteFile(spec, []byte(specPath), 0644); err != nil {
			return err
		}
	}

	return nil
}

func (m *mod) Stop() error {
	return nil
}

func (m *mod) Name() string {
	return m.name
}

func (m *mod) Description() string {
	return m.desc
}

func (m *mod) Address() string {
	return m.addr
}

func (m *mod) buildMux() *http.ServeMux {

	mux := http.NewServeMux()

	mux.HandleFunc("/Plugin.Activate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1.2+json")
		fmt.Fprintln(w, `{"Implements": ["VolumeDriver"]}`)
	})

	mux.HandleFunc("/VolumeDriver.Create", func(w http.ResponseWriter, r *http.Request) {
		var pr pluginRequest
		if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err).Error("/VolumeDriver.Create: error decoding json")
			return
		}

		err := m.r.Volume.Create(pr.Name, pr.Opts)
		if err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err.Error()).Error("/VolumeDriver.Create: error creating volume")
			log.Error(err)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1.2+json")
		fmt.Fprintln(w, `{}`)
	})

	mux.HandleFunc("/VolumeDriver.Remove", func(w http.ResponseWriter, r *http.Request) {
		var pr pluginRequest
		if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err).Error("/VolumeDriver.Remove: error decoding json")
			return
		}

		err := m.r.Volume.Remove(pr.Name)
		if err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err.Error()).Error("/VolumeDriver.Remove: error removing volume")
			log.Error(err)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1.2+json")
		fmt.Fprintln(w, `{}`)
	})

	mux.HandleFunc("/VolumeDriver.Path", func(w http.ResponseWriter, r *http.Request) {
		var pr pluginRequest
		if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err).Error("/VolumeDriver.Path: error decoding json")
			return
		}

		mountPath, err := m.r.Volume.Path(pr.Name, "")
		if err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err.Error()).Error("/VolumeDriver.Path: error returning path")
			log.Error(err)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1.2+json")
		fmt.Fprintln(w, fmt.Sprintf("{\"Mountpoint\": \"%s\"}", mountPath))
	})

	mux.HandleFunc("/VolumeDriver.Mount", func(w http.ResponseWriter, r *http.Request) {
		var pr pluginRequest
		if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err).Error("/VolumeDriver.Mount: error decoding json")
			return
		}

		mountPath, err := m.r.Volume.Mount(pr.Name, "", false, "", false)
		if err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err.Error()).Error("/VolumeDriver.Mount: error mounting volume")
			log.Error(err)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1.2+json")
		fmt.Fprintln(w, fmt.Sprintf("{\"Mountpoint\": \"%s\"}", mountPath))
	})

	mux.HandleFunc("/VolumeDriver.Unmount", func(w http.ResponseWriter, r *http.Request) {
		var pr pluginRequest
		if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err).Error("/VolumeDriver.Unmount: error decoding json")
			return
		}

		err := m.r.Volume.Unmount(pr.Name, "")
		if err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err.Error()).Error("/VolumeDriver.Unmount: error unmounting volume")
			log.Error(err)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1.2+json")
		fmt.Fprintln(w, `{}`)
	})

	mux.HandleFunc("/VolumeDriver.Get", func(w http.ResponseWriter, r *http.Request) {
		var pr pluginRequest
		if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err).Error("/VolumeDriver.Path: error decoding json")
			return
		}

		vol, err := m.r.Volume.Get(pr.Name)
		if err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err.Error()).Error("/VolumeDriver.Get: error getting volume")
			log.Error(err)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1.2+json")
		json.NewEncoder(w).Encode(map[string]core.VolumeMap{"Volume": vol})
	})

	mux.HandleFunc("/VolumeDriver.List", func(w http.ResponseWriter, r *http.Request) {
		var pr pluginRequest
		if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err).Error("/VolumeDriver.List: error decoding json")
			return
		}

		volList, err := m.r.Volume.List()
		if err != nil {
			http.Error(w, fmt.Sprintf("{\"Error\":\"%s\"}", err.Error()), 500)
			log.WithField("error", err.Error()).Error("/VolumeDriver.List: error listing volumes")
			log.Error(err)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1.2+json")
		json.NewEncoder(w).Encode(map[string][]core.VolumeMap{"Volumes": volList})
	})

	return mux
}
