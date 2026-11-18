### MESA RustGIS

#### Description and Quick Start

[RustGIS](https://github.com/opengeos/RustGIS) is a GIS that runs in the browser. This app serves RustGIS 0.6.0 together with a **Data Store** panel, so maps can be built from files in your CyVerse Data Store without downloading them first.

1. Launch the app with **Mount data store in analysis** left on (the default), and open the analysis when it is ready. The map opens in a browser tab.
2. Click the **Data Store** tab on the right edge. The panel lists five locations:
   - **Home**: `/iplant/home/<your username>` (read and write)
   - **Community Data**: `/iplant/home/shared` (read only)
   - **Analysis Inputs** (read only) and **Analysis Outputs** (read and write)
   - **Session Scratch**: temporary space that is removed when the analysis ends
3. Tick up to 16 files and choose **Open in new map**. This replaces the current map. For a Shapefile tick the `.shp` file; its `.dbf`, `.shx`, `.prj` and `.cpg` files are read with it.
4. To open a saved project, tick one `.rgis` or `.rgispkg` file on its own and choose **Open project**.
5. **Copy URL** gives an address for one file that works in RustGIS's own *Add Data › From URL* dialog, in this analysis only.
6. **Upload** copies files from your computer into the folder shown, and **New folder** creates one. Both are available in Home, Analysis Outputs and Session Scratch. Large uploads are sent in pieces and resume if the connection drops.

Good to know:

- The app needs **Mount data store in analysis**. If it was turned off at launch, the panel shows no Data Store locations and says so; launch the app again with the option on. (In that mode Session Scratch is not temporary either: the Discovery Environment copies it to the analysis's output folder at the end.)
- Cloud-Optimized GeoTIFF, FlatGeobuf, GeoParquet and PMTiles files are read in place, a piece at a time, so large files open quickly. Other formats are downloaded whole by the browser, up to 256 MiB per file.
- RustGIS's own Save and Export write to your computer's downloads folder. To keep a project in the Data Store, save it and then upload it with the panel.
- A file whose name has spaces or accented letters opens under an encoded layer name such as `my%20data`. Rename the layer in the Layers panel, or avoid those characters in file names.
- Files cannot be deleted or renamed from the panel. Use the Discovery Environment's Data window for that.
- Files and folders whose names begin with a dot, and symbolic links, are not shown.
- If an upload ends with "The Data Store refused or failed the operation", check that you may write to that folder and that you are within your quota.
- Preferences and saved connections live in the browser and start empty in every new analysis.
- Anyone you share the analysis with can read and write the same folders as you. Use **MESA RustGIS (read-only)** for analyses you intend to share.

#### Test Data

Any GeoJSON, CSV with coordinate columns, GeoTIFF or FlatGeobuf file in your Data Store will do. Public data sets are under **Community Data**.

#### Input File(s)

None are required. Files chosen as analysis inputs appear under **Analysis Inputs** in the panel.

#### Parameters Used in App

None.

#### Output File(s)

Files uploaded or created under **Analysis Outputs** are kept with the analysis. Files written to **Home** are in your Data Store immediately.

### License

The app is Apache-2.0: image `harbor.cyverse.org/vice/mesa-rustgis`, source <https://github.com/idss-mesa/mesa-rustgis-app>. RustGIS is Apache-2.0, copyright its authors; its third-party licences are served by the app at `/THIRD-PARTY-LICENSES.txt`.

### Reference(s)

- RustGIS: <https://github.com/opengeos/RustGIS>
- CyVerse VICE: <https://learning.cyverse.org/vice/>
