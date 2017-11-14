package gpkg

import (
	log "github.com/sirupsen/logrus"
	"github.com/terranodo/tegola"
	"github.com/terranodo/tegola/basic"
	"github.com/terranodo/tegola/mvt"
	"github.com/terranodo/tegola/mvt/provider"
	"github.com/terranodo/tegola/util"
	"github.com/terranodo/tegola/util/dict"
	"github.com/terranodo/tegola/wkb"

	_ "github.com/mattn/go-sqlite3"

	"database/sql"

	"bytes"
	"context"
	"fmt"
)

const (
	ProviderName = "gpkg"
	FilePath     = "FilePath"
	DefaultSRID  = tegola.WebMercator
)

// layer holds information about a query.
// Currently stolen exactly from provider.postgis.layer
type layer struct {
	// The Name of the layer
	name string
	// The SQL to use when querying PostGIS for this layer
	sql string
	// The ID field name, this will default to 'gid' if not set to something other then empty string.
	idField string
	// The Geometery field name, this will default to 'geom' if not set to soemthing other then empty string.
	geomField string
	// GeomType is the the type of geometry returned from the SQL
	geomType tegola.Geometry
	// The SRID that the data in the table is stored in. This will default to WebMercator
	srid int
}

type GPKGProvider struct {
	// Currently just the path to the gpkg file.
	FilePath string
	// map of layer name and corrosponding sql
	layers map[string]layer
	srid   int
}

type LayerInfo interface {
	Name() string
	GeomType() tegola.Geometry
	SRID() int
}

// Implements mvt.LayerInfo interface
type GPKGLayer struct {
	name     string
	geomtype tegola.Geometry
	srid     int
}

func (l GPKGLayer) Name() string              { return l.name }
func (l GPKGLayer) GeomType() tegola.Geometry { return l.geomtype }
func (l GPKGLayer) SRID() int                 { return l.srid }

func (p *GPKGProvider) Layers() ([]mvt.LayerInfo, error) {
	util.CodeLogger.Debug("Attempting gpkg.Layers()")
	layerCount := len(p.layers)
	ls := make([]mvt.LayerInfo, layerCount)

	i := 0
	for _, player := range p.layers {
		l := GPKGLayer{name: player.name, srid: player.srid, geomtype: player.geomType}
		ls[i] = l
		i++
	}

	util.CodeLogger.Debugf("Ok, returning mvt.LayerInfo array: %v", ls)
	return ls, nil
}

func (p *GPKGProvider) MVTLayer(ctx context.Context, layerName string, tile tegola.Tile, tags map[string]interface{}) (*mvt.Layer, error) {
	util.CodeLogger.Debugf("GPKGProvider MVTLayer() called for %v", layerName)
	filepath := p.FilePath

	util.CodeLogger.Infof("Opening gpkg at: ", filepath)
	db, err := sql.Open("sqlite3", filepath)
	if err != nil {
		return nil, err
	}

	// Get all feature rows for the layer requested.
	qtext := fmt.Sprintf("SELECT * FROM %v WHERE geom IS NOT NULL;", layerName)
	rows, err := db.Query(qtext)
	if err != nil {
		util.CodeLogger.Errorf("Error during query: %v - %v", qtext, err)
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	vals := make([]interface{}, len(cols))
	valPtrs := make([]interface{}, len(cols))
	for i := 0; i < len(cols); i++ {
		valPtrs[i] = &vals[i]
	}

	pLayer := p.layers[layerName]
	newLayer := new(mvt.Layer)
	newLayer.Name = layerName

	rowCount := 0
	var geom tegola.Geometry
	for rows.Next() {
		geom = nil
		rowCount++
		err = rows.Scan(valPtrs...)
		if err != nil {
			util.CodeLogger.Error(err)
			continue
		}
		var gid uint64

		for i := 0; i < len(cols); i++ {
			if cols[i] == "geom" {
				util.CodeLogger.Debugf("Doing gpkg geometry extraction...", vals[i])
				var h GeoPackageBinaryHeader
				geomData := vals[i].([]byte)
				h.Init(geomData)

				reader := bytes.NewReader(geomData[h.Size():])
				geom, err = wkb.Decode(reader)

				if err != nil {
					util.CodeLogger.Errorf("Error decoding geometry: %v", err)
				}

				if h.SRSId() != DefaultSRID {
					util.CodeLogger.Infof("SRID %v != %v, trying to convert...", pLayer.srid, DefaultSRID)
					// We need to convert our points to Webmercator.
					g, err := basic.ToWebMercator(pLayer.srid, geom)
					if err != nil {
						util.CodeLogger.Errorf(
							"Was unable to transform geometry to webmercator from "+
								"SRID (%v) for layer (%v) due to error: %v",
							pLayer.srid, layerName, err)
						return nil, err
					} else {
						util.CodeLogger.Info("...conversion ok")
					}
					geom = g.Geometry
				} else {
					util.CodeLogger.Infof("SRID already default (%v), no conversion necessary", DefaultSRID)
				}
			}
		}

		if geom == nil {
			util.CodeLogger.Warn("No geometry in row, skipping feature")
			continue
		}

		f := mvt.Feature{
			ID:       &gid,
			Tags:     make(map[string]interface{}),
			Geometry: geom,
		}
		newLayer.AddFeatures(f)
	}

	if rowCount != len(newLayer.Features()) {
		util.CodeLogger.Errorf("newLayer feature count doesn't match table row count (%v != %v)\n",
			len(newLayer.Features()), rowCount)
	}
	return newLayer, nil
}

func NewProvider(config map[string]interface{}) (mvt.Provider, error) {
	m := dict.M(config)
	filepath, err := m.String("FilePath", nil)
	if err != nil {
		util.CodeLogger.Error(err)
		return nil, err
	}

	util.CodeLogger.Debug("Attempting sql.Open() w/ filepath: ", filepath)
	db, err := sql.Open("sqlite3", filepath)
	if err != nil {
		util.CodeLogger.Errorf("Error opening gpkg file: %v", err)
		return nil, err
	}

	p := GPKGProvider{FilePath: filepath, layers: make(map[string]layer)}

	qtext := "SELECT * FROM gpkg_contents"
	rows, err := db.Query(qtext)
	if err != nil {
		util.CodeLogger.Errorf("Error during query: %v - %v", qtext, err)
		return nil, err
	}
	defer rows.Close()

	var tablename string
	var srid int
	var ignore string

	logMsg := "gpkg_contents: "
	var geomData []byte

	for rows.Next() {
		rows.Scan(&tablename, &ignore, &ignore, &ignore, &ignore, &ignore, &ignore, &ignore, &ignore, &srid)

		// Get layer geometry as geometry of first feature in table
		geomQtext := fmt.Sprintf("SELECT geom FROM %v LIMIT 1;", tablename)
		geomRow := db.QueryRow(geomQtext)
		geomRow.Scan(&geomData)
		var h GeoPackageBinaryHeader
		h.Init(geomData)

		reader := bytes.NewReader(geomData[h.Size():])
		geom, err := wkb.Decode(reader)

		if err != nil {
			util.CodeLogger.Errorf("Error decoding geometry: %v", err)
		}

		log.Infof("Got Geometry type %T for table %v", geom, tablename)
		layerQuery := fmt.Sprintf("SELECT * FROM %v;", tablename)
		p.layers[tablename] = layer{name: tablename, sql: layerQuery, geomType: geom, srid: srid}

		//		// The ID field name, this will default to 'gid' if not set to something other then empty string.
		//		idField string
		//		// The Geometery field name, this will default to 'geom' if not set to soemthing other then empty string.
		//		geomField string
		//		// GeomType is the the type of geometry returned from the SQL

		var logMsgPart string
		fmt.Sprintf(logMsgPart, "(%v-%i) ", tablename, srid)
		logMsg += logMsgPart
	}
	util.CodeLogger.Debug(logMsg)

	return &p, err
}

func (p *GPKGProvider) layerGeomType(l *layer) {
	msg := "GPKGProvider.layerGeomType() called (not implemented)"
	fmt.Println(msg)
	util.CodeLogger.Debug(msg)
}

func init() {
	util.CodeLogger.Debug("Entering gpkgProvider.go init()")
	provider.Register(ProviderName, NewProvider)
}
