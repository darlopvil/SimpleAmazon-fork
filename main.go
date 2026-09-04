package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Noooste/azuretls-client"
	"github.com/PuerkitoBio/goquery"
)

var (
	//go:embed templates
	plantillasFS embed.FS

	//go:embed static
	staticFiles embed.FS
)

// Opcion del selector de ordenacion. Antes las cinco opciones estaban escritas
// a mano en cada plantilla, con un if/else por opcion para marcar la elegida.
type opcionOrden struct {
	Valor    string
	Etiqueta string
}

var opcionesOrden = []opcionOrden{
	{"", "Featured"},
	{"price-asc-rank", "Price: Low to High"},
	{"price-desc-rank", "Price: High to Low"},
	{"review-rank", "Avg. Customer Review"},
	{"date-desc-rank", "Newest Arrivals"},
}

var funcionesPlantilla = template.FuncMap{
	"substract": func(a, b int) int {
		return a - b
	},
	"add": func(a, b int) int {
		return a + b
	},
	"opcionesOrden": func() []opcionOrden {
		return opcionesOrden
	},
}

// Cada pagina se parsea en su propio conjunto: la plantilla base, las parciales
// comunes y el fichero que define el bloque "contenido". Un unico conjunto no
// serviria, porque las tres paginas definen ese mismo bloque.
func cargarPlantilla(pagina string) *template.Template {
	return template.Must(template.New("base.html").Funcs(funcionesPlantilla).ParseFS(
		plantillasFS,
		"templates/base.html",
		"templates/formulario.html",
		"templates/paginacion.html",
		"templates/"+pagina,
	))
}

type SearchResult struct {
	// Precio de referencia tachado y condicion de acceso, tal como los
	// publica Amazon. Un importe de cero corresponde casi siempre a un
	// audiolibro o a un titulo incluido en una suscripcion, y sin esa
	// explicacion resulta enganoso.
	PrecioTachado string
	Condicion     string
	Title         string
	URL           string
	Price         string
	ImageURL      string
	Type          string
	Ratings       string
	Reviews       int
	LimitedSupply string
}

type SearchResults struct {
	Idioma string
	// Codigo de estado HTTP con el que responder cuando Error no esta vacio.
	// Sin el, todas las rutas de error respondian 200 y ni el usuario ni una
	// comprobacion automatizada podian distinguir un fallo de un exito.
	Estado  int
	Pages   int
	Page    int
	TLD     string
	Sort    string
	Query   string
	Results []SearchResult
	Error   string
}

type TemplateValues struct {
	Sort   string
	TLD    string
	Query  string
	Idioma string
}

// Codigo de idioma para el atributo lang del documento, derivado del TLD. Antes
// estaba fijado a "en" en las tres plantillas, con independencia del
// marketplace consultado.
func idiomaHTML(tld string) string {
	cabecera, ok := idiomaPorTLD[tld]
	if !ok {
		return "en"
	}
	if i := strings.Index(cabecera, ","); i > 0 {
		return cabecera[:i]
	}
	return cabecera
}

/*
0: Featured: None
1: Price: Low to High: price-asc-rank
2: Price: High to Low: price-desc-rank
3: Avg. Customer Review: review-rank
4: Newest Arrivals: date-desc-rank
*/

// Las plantillas se parsean una sola vez, al inicializar el paquete. La de
// resultados se volvia a parsear en cada peticion, junto con su mapa de
// funciones. template.Must aborta el arranque si alguna es invalida, en lugar
// de dejar un puntero nulo con el que la primera peticion provocaria un panic.
var (
	plantillaIndex      = cargarPlantilla("index.html")
	plantillaUnhandled  = cargarPlantilla("unhandled.html")
	plantillaResultados = cargarPlantilla("searchResults.html")
	plantillaError      = cargarPlantilla("error.html")
	plantillaProducto   = cargarPlantilla("product.html")
)

// User-Agent de una version reciente de Firefox. El valor anterior correspondia
// a Firefox 47, de 2016, lo que destaca de inmediato ante cualquier heuristica
// de deteccion. Conviene revisarlo de vez en cuando.
const userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

// Idioma que se solicita a cada marketplace, para que los textos de los
// resultados lleguen en el idioma del dominio elegido y no en el que Amazon
// deduzca por geolocalizacion. Los precios no se ven afectados por esta
// cabecera: Amazon los localiza por IP, comprobado enviando es-ES y ja-JP a
// amazon.com y obteniendo el mismo importe en euros en ambos casos.
var idiomaPorTLD = map[string]string{
	"ae": "ar-AE,ar;q=0.9", "be": "fr-BE,fr;q=0.9", "ca": "en-CA,en;q=0.9",
	"cl": "es-CL,es;q=0.9", "cn": "zh-CN,zh;q=0.9", "co.jp": "ja-JP,ja;q=0.9",
	"co.uk": "en-GB,en;q=0.9", "co.za": "en-ZA,en;q=0.9", "com": "en-US,en;q=0.9",
	"com.au": "en-AU,en;q=0.9", "com.br": "pt-BR,pt;q=0.9", "com.mx": "es-MX,es;q=0.9",
	"com.ng": "en-NG,en;q=0.9", "com.tr": "tr-TR,tr;q=0.9", "de": "de-DE,de;q=0.9",
	"eg": "ar-EG,ar;q=0.9", "es": "es-ES,es;q=0.9", "fr": "fr-FR,fr;q=0.9",
	"ie": "en-IE,en;q=0.9", "in": "en-IN,en;q=0.9", "it": "it-IT,it;q=0.9",
	"nl": "nl-NL,nl;q=0.9", "pl": "pl-PL,pl;q=0.9", "sa": "ar-SA,ar;q=0.9",
	"se": "sv-SE,sv;q=0.9", "sg": "en-SG,en;q=0.9",
}

// Cache de resultados en memoria. Sin ella, cada carga de pagina se traduce en
// una peticion nueva a Amazon aunque se repita la misma busqueda, lo que eleva
// el volumen de trafico saliente y con el la probabilidad de acabar recibiendo
// una verificacion antibot.
//
// El TTL es corto a proposito: los precios cambian y no interesa servir datos
// rancios.
const (
	ttlCache         = 10 * time.Minute
	maxEntradasCache = 256
)

type entradaCache struct {
	resultado SearchResults
	expira    time.Time
}

var (
	cacheMu sync.Mutex
	cache   = map[string]entradaCache{}
)

func claveCache(tld string, searchTerm string, page int, sort string) string {
	// El separador nulo evita que combinaciones distintas produzcan la misma
	// clave al concatenarse.
	return tld + "\x00" + searchTerm + "\x00" + strconv.Itoa(page) + "\x00" + sort
}

// Caché de fichas de producto. Comparte el mismo límite y la misma expiración
// que la de búsquedas, pero necesita su propio almacén porque el tipo guardado
// es distinto.
type entradaProducto struct {
	ficha  Producto
	expira time.Time
}

var (
	cacheProductoMu sync.Mutex
	almacenProducto = map[string]entradaProducto{}
)

func cacheProducto(clave string) (Producto, bool) {
	cacheProductoMu.Lock()
	defer cacheProductoMu.Unlock()
	e, ok := almacenProducto[clave]
	if !ok {
		return Producto{}, false
	}
	if time.Now().After(e.expira) {
		delete(almacenProducto, clave)
		return Producto{}, false
	}
	return e.ficha, true
}

func cacheSetProducto(clave string, ficha Producto) {
	cacheProductoMu.Lock()
	defer cacheProductoMu.Unlock()
	if len(almacenProducto) >= maxEntradasCache {
		ahora := time.Now()
		for k, e := range almacenProducto {
			if ahora.After(e.expira) {
				delete(almacenProducto, k)
			}
		}
		if len(almacenProducto) >= maxEntradasCache {
			almacenProducto = map[string]entradaProducto{}
		}
	}
	almacenProducto[clave] = entradaProducto{ficha: ficha, expira: time.Now().Add(ttlCache)}
}

func cacheGet(clave string) (SearchResults, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	e, ok := cache[clave]
	if !ok {
		return SearchResults{}, false
	}
	if time.Now().After(e.expira) {
		delete(cache, clave)
		return SearchResults{}, false
	}
	return e.resultado, true
}

func cacheSet(clave string, resultado SearchResults) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if len(cache) >= maxEntradasCache {
		ahora := time.Now()
		for k, e := range cache {
			if ahora.After(e.expira) {
				delete(cache, k)
			}
		}
		// Si la purga de caducadas no libera nada, se descarta todo: acotar la
		// memoria importa mas que conservar entradas concretas.
		if len(cache) >= maxEntradasCache {
			cache = map[string]entradaCache{}
		}
	}
	cache[clave] = entradaCache{resultado: resultado, expira: time.Now().Add(ttlCache)}
}

// Limite de lectura del cuerpo de la respuesta. Una pagina de resultados ronda
// los 2 MB; el margen cubre las mas cargadas sin permitir que una respuesta
// anomala agote la memoria del proceso.
const maxCuerpo = 8 << 20

// Limite del cuerpo que el proxy de imagenes reenvia al cliente. Las miniaturas
// de una pagina de resultados no llegan a los 100 KB; el margen es amplio y aun
// asi acota lo que un solo recurso puede transferir.
const maxImagen = 5 << 20

// Marcadores de las paginas que Amazon sirve con codigo 200 en lugar de
// resultados. bm-verify pertenece al interstitial de Akamai Bot Manager, que
// exige ejecutar JavaScript; el resto, a la pagina de captcha clasica.
var marcadoresBloqueo = [][]byte{
	[]byte("bm-verify"),
	[]byte("/errors/validateCaptcha"),
	[]byte("api-services-support@amazon.com"),
}

func esBloqueo(cuerpo []byte) bool {
	for _, m := range marcadoresBloqueo {
		if bytes.Contains(cuerpo, m) {
			return true
		}
	}
	return false
}

// Servicio de inspeccion usado por el modo diagnostico para averiguar con que
// huella TLS y HTTP/2 se presenta el cliente ante un servidor.
const urlHuella = "https://tls.peet.ws/api/clean"

// Consulta y muestra la huella del cliente del programa. Amazon esta detras de
// un servicio antibot que puntua la huella de la conexion, no solo las
// cabeceras, asi que hace falta poder medirla para compararla con la de un
// navegador real y verificar el efecto de cualquier cambio en el transporte.
func imprimirHuella() {
	ctx, cancelar := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelar()

	resp, err := obtener(ctx, urlHuella, [][2]string{{"User-Agent", userAgent}}, 1<<20)
	if err != nil {
		fmt.Println("no se pudo consultar la huella:", err)
		os.Exit(1)
	}

	fmt.Printf("protocolo negociado: %s\nuser-agent enviado: %s\n\n%s\n", resp.Proto, userAgent, resp.Cuerpo)
}

// Sesion que imita la huella de un navegador. Se crea solo si se solicita por
// linea de ordenes; mientras sea nil se usa el cliente estandar.
//
// Amazon esta detras de un servicio antibot que puntua la huella de la conexion
// en dos ejes independientes: el saludo TLS y el perfil de tramas HTTP/2. El
// cliente estandar de Go tiene ambos catalogados, y actuar solo sobre uno no
// sirve de nada, porque el fingerprint de HTTP/2 no depende de las cifras ni de
// las extensiones TLS.
var sesionNavegador *azuretls.Session

// Huella medida de un curl que atraviesa el servicio antibot desde la misma
// maquina y en el mismo momento en que la aplicacion es rechazada. Sirve para
// comprobar si el criterio de bloqueo es la huella o alguna otra senal.
const ja3Curl = "771,4866-4867-4865-49196-49200-159-52393-52392-52394-49195-49199-158-49188-49192-107-49187-49191-103-49162-49172-57-49161-49171-51-157-156-61-60-53-47,65281-0-11-10-16-22-23-49-13-43-45-51-27,4588-29-23-30-24-25-256-257,0-1-2"
const akamaiCurl = "3:100,4:65536,2:0|1048510465|0|m,s,a,p"

// Respuesta minima comun a los dos clientes. Cada uno usa tipos incompatibles
// entre si, de modo que las peticiones salientes pasan por esta capa en lugar
// de manejar directamente uno u otro.
type respuestaSaliente struct {
	Estado   int
	Proto    string
	Cabecera func(string) string
	Cuerpo   []byte
}

// Realiza una peticion saliente con el cliente activo. Las cabeceras se pasan
// en orden porque su secuencia es una de las senales que el servicio antibot
// inspecciona.
func obtener(ctx context.Context, destino string, cabeceras [][2]string, limite int64) (*respuestaSaliente, error) {
	if sesionNavegador != nil {
		ordenadas := make(azuretls.OrderedHeaders, 0, len(cabeceras))
		for _, c := range cabeceras {
			ordenadas = append(ordenadas, []string{c[0], c[1]})
		}

		resp, err := sesionNavegador.Do(&azuretls.Request{
			Method:         "GET",
			Url:            destino,
			OrderedHeaders: ordenadas,
		})
		if err != nil {
			return nil, err
		}

		cuerpo := resp.Body
		if int64(len(cuerpo)) > limite {
			cuerpo = cuerpo[:limite]
		}
		return &respuestaSaliente{
			Estado:   resp.StatusCode,
			Proto:    "HTTP/2.0",
			Cabecera: resp.Header.Get,
			Cuerpo:   cuerpo,
		}, nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", destino, nil)
	if err != nil {
		return nil, err
	}
	for _, c := range cabeceras {
		req.Header.Set(c[0], c[1])
	}

	res, err := clienteHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	cuerpo, err := io.ReadAll(io.LimitReader(res.Body, limite))
	if err != nil {
		return nil, err
	}
	return &respuestaSaliente{
		Estado:   res.StatusCode,
		Proto:    res.Proto,
		Cabecera: res.Header.Get,
		Cuerpo:   cuerpo,
	}, nil
}

// Valor de una variable de entorno, o el valor por defecto si no está definida.
// El programa admite configuración por entorno además de por flags, porque en un
// contenedor lo habitual es lo primero.
func entorno(nombre string, porDefecto string) string {
	if v, ok := os.LookupEnv(nombre); ok && v != "" {
		return v
	}
	return porDefecto
}

// Cliente compartido para todas las peticiones salientes. El cliente por
// defecto de net/http no tiene timeout, asi que una respuesta que nunca llega
// deja la goroutine que la espera colgada de forma indefinida.
var clienteHTTP = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		// El Transport por defecto de net/http lo activa; al construir uno a
		// medida hay que fijarlo de forma explicita o las peticiones salientes
		// caen a HTTP/1.1 sin avisar.
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        20,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	},
}

// TLDs de los marketplaces que opera Amazon. El valor llega del usuario y se
// concatena en la URL de destino, asi que se valida contra esta lista cerrada:
// sin ella, un valor como "com@ejemplo.org" dirige la peticion saliente a un
// host arbitrario.
var tldsPermitidos = map[string]bool{
	"ae": true, "be": true, "ca": true, "cl": true, "cn": true,
	"co.jp": true, "co.uk": true, "co.za": true, "com": true,
	"com.au": true, "com.br": true, "com.mx": true, "com.ng": true,
	"com.tr": true, "de": true, "eg": true, "es": true, "fr": true,
	"ie": true, "in": true, "it": true, "nl": true, "pl": true,
	"sa": true, "se": true, "sg": true,
}

// Criterios de ordenacion admitidos por Amazon. La cadena vacia corresponde al
// orden por defecto, el que muestra la interfaz como "Featured".
var ordenesPermitidos = map[string]bool{
	"":                true,
	"price-asc-rank":  true,
	"price-desc-rank": true,
	"review-rank":     true,
	"date-desc-rank":  true,
}

// Amazon no pagina indefinidamente. Pedir mas alla de este limite no aporta
// resultados y si una peticion saliente inutil.
const paginaMaxima = 20

// Hosts desde los que Amazon sirve las imagenes de los resultados. El proxy
// solo reenvia peticiones a estos, y el host viaja dentro de la propia ruta
// para no tener que darlo por supuesto.
var cdnsImagen = map[string]bool{
	"m.media-amazon.com":              true,
	"images-na.ssl-images-amazon.com": true,
	"images-eu.ssl-images-amazon.com": true,
}

// Convierte la URL de una imagen en una ruta del proxy. Antes se recortaba un
// numero fijo de caracteres, correcto solo para m.media-amazon.com; con los
// otros dos hosts el corte caia en mitad del dominio y producia una ruta sin
// sentido que el proxy reenviaba igualmente.
func rutaProxyImagen(src string) string {
	u, err := url.Parse(src)
	if err != nil || !cdnsImagen[u.Host] {
		return ""
	}
	return "/mediaproxy/" + u.Host + u.Path
}

// Formato de un producto con su precio. Amazon presenta las tarjetas con varias
// ediciones como una lista de bloques dentro del mismo contenedor, separados
// por un divisor.
type formatoPrecio struct {
	Formato string
	Precio  string
}

// Importe numérico de un precio, para poder compararlos entre sí. Se aceptan
// tanto el punto como la coma decimal, porque el formato varía según el
// marketplace.
func importePrecio(precio string) (float64, bool) {
	var digitos strings.Builder
	for _, r := range precio {
		switch {
		case r >= '0' && r <= '9':
			digitos.WriteRune(r)
		case r == ',' || r == '.':
			digitos.WriteRune('.')
		}
	}
	texto := digitos.String()
	// El último separador es el decimal; los anteriores son de millares.
	if i := strings.LastIndex(texto, "."); i >= 0 {
		texto = strings.ReplaceAll(texto[:i], ".", "") + texto[i:]
	}
	valor, err := strconv.ParseFloat(texto, 64)
	return valor, err == nil
}

// Empareja cada formato con su precio dentro de la tarjeta. Antes se tomaba el
// primer precio suelto, que podía ser el de una edición irrelevante, y el
// nombre del formato se obtenía con Text sobre toda la selección, lo que
// concatenaba los de todos los formatos sin separador.
//
// Se excluyen los precios marcados como a-text-price, que corresponden al
// importe tachado de referencia y al precio por unidad.
func formatosYPrecios(result *goquery.Selection) []formatoPrecio {
	bloque := result.Find("[data-cy='price-recipe']").First()

	var pares []formatoPrecio
	bloque.Find("a.a-text-bold, span.a-price:not(.a-text-price) > span.a-offscreen").
		Each(func(i int, s *goquery.Selection) {
			texto := strings.TrimSpace(s.Text())
			if goquery.NodeName(s) == "a" {
				pares = append(pares, formatoPrecio{Formato: texto})
				return
			}
			if len(pares) > 0 && pares[len(pares)-1].Precio == "" {
				pares[len(pares)-1].Precio = texto
				return
			}
			pares = append(pares, formatoPrecio{Precio: texto})
		})
	return pares
}

// Precio de referencia tachado, si Amazon lo publica.
func precioTachado(result *goquery.Selection) string {
	return strings.TrimSpace(result.Find(
		"[data-cy='price-recipe'] span.a-price.a-text-price[data-a-strike='true'] > span.a-offscreen").
		First().Text())
}

// Condicion de acceso al producto cuando su importe es cero. Amazon lo indica
// con textos del tipo "Gratis con la prueba de Audible" o "Incluido con una
// suscripcion Kindle Unlimited". Sin ese dato, un precio de cero se confunde
// con un producto gratuito o no disponible.
func condicionAcceso(result *goquery.Selection) string {
	var condicion string
	result.Find("[data-cy='price-recipe']").First().Find("span").
		EachWithBreak(func(i int, s *goquery.Selection) bool {
			texto := strings.Join(strings.Fields(s.Text()), " ")
			minusculas := strings.ToLower(texto)
			if len(texto) < 90 &&
				(strings.Contains(minusculas, "audible") ||
					strings.Contains(minusculas, "kindle unlimited")) {
				condicion = texto
				return false
			}
			return true
		})
	return condicion
}

// Longitud del identificador de producto de Amazon. Comprobado sobre 127
// identificadores de dos categorías distintas: todos miden lo mismo.
const longitudASIN = 10

// Comprueba que el identificador tiene el formato que usa Amazon: diez
// caracteres alfanuméricos en mayúsculas. Los libros terminan a veces en X,
// porque su identificador es un ISBN de diez dígitos.
func asinValido(asin string) bool {
	if len(asin) != longitudASIN {
		return false
	}
	for _, r := range asin {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			continue
		}
		return false
	}
	return true
}

// Extrae el identificador de producto de una URL de Amazon, si lo lleva. Las
// fichas siguen el patrón /dp/{asin}, a veces precedido del nombre del producto.
func asinDeURL(enlace string) string {
	i := strings.Index(enlace, "/dp/")
	if i < 0 {
		return ""
	}
	resto := enlace[i+len("/dp/"):]
	if j := strings.IndexAny(resto, "/?#"); j >= 0 {
		resto = resto[:j]
	}
	if !asinValido(resto) {
		return ""
	}
	return resto
}

// Imagen de la galería, con la versión grande y la miniatura, ambas servidas a
// través del proxy.
type Imagen struct {
	Grande string
	Mini   string
	Alt    string
}

// Par etiqueta/valor de la ficha técnica.
type Especificacion struct {
	Etiqueta string
	Valor    string
}

// Amazon incrusta la galería completa como JSON dentro de un script, con todas
// las resoluciones de cada imagen. Es más fiable que recorrer las miniaturas
// del documento.
var reGaleria = regexp.MustCompile(`'colorImages'\s*:\s*\{\s*'initial'\s*:\s*A\.\$\.parseJSON\('(.*?)'\)`)

func galeria(cuerpo []byte) []Imagen {
	m := reGaleria.FindSubmatch(cuerpo)
	if m == nil {
		return nil
	}

	// El JSON viaja dentro de una cadena de JavaScript, con las secuencias de
	// escape sin resolver.
	crudo, err := strconv.Unquote(`"` + strings.ReplaceAll(string(m[1]), `"`, `\"`) + `"`)
	if err != nil {
		return nil
	}

	var entradas []struct {
		HiRes   string `json:"hiRes"`
		Large   string `json:"large"`
		Thumb   string `json:"thumb"`
		AltText string `json:"altText"`
	}
	if err := json.Unmarshal([]byte(crudo), &entradas); err != nil {
		return nil
	}

	var imagenes []Imagen
	for _, e := range entradas {
		grande := e.Large
		if grande == "" {
			grande = e.HiRes
		}
		ruta := rutaProxyImagen(grande)
		if ruta == "" {
			continue
		}
		imagenes = append(imagenes, Imagen{
			Grande: ruta,
			Mini:   rutaProxyImagen(e.Thumb),
			Alt:    e.AltText,
		})
	}
	return imagenes
}

// Limpia las marcas invisibles de dirección de texto y los separadores que
// Amazon intercala en las etiquetas de la ficha técnica.
func limpiarEtiqueta(t string) string {
	t = strings.Map(func(r rune) rune {
		if r == '\u200e' || r == '\u200f' {
			return -1
		}
		return r
	}, t)
	t = strings.Join(strings.Fields(t), " ")
	return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(t), ":"))
}

// Ficha técnica del producto. Amazon la publica en dos bloques con estructuras
// distintas: una tabla de resumen y una lista de detalles.
func especificaciones(doc *goquery.Document) []Especificacion {
	var specs []Especificacion
	vistas := map[string]bool{}

	// Amazon repite en la ficha técnica datos que ya se muestran por separado,
	// y alguno llega mal formateado. Se descartan por nombre.
	descartadas := map[string]bool{
		"Opiniones de los clientes": true,
		"Customer Reviews":          true,
	}

	anadir := func(etiqueta, valor string) {
		etiqueta = limpiarEtiqueta(etiqueta)
		valor = strings.Join(strings.Fields(valor), " ")
		if etiqueta == "" || valor == "" || vistas[etiqueta] || descartadas[etiqueta] {
			return
		}
		vistas[etiqueta] = true
		specs = append(specs, Especificacion{Etiqueta: etiqueta, Valor: valor})
	}

	doc.Find("#productOverview_feature_div tr").Each(func(i int, tr *goquery.Selection) {
		celdas := tr.Find("td")
		if celdas.Length() >= 2 {
			anadir(celdas.Eq(0).Text(), celdas.Eq(1).Text())
		}
	})

	doc.Find("#detailBullets_feature_div li span.a-list-item").Each(func(i int, li *goquery.Selection) {
		spans := li.Find("span")
		if spans.Length() >= 2 {
			anadir(spans.Eq(0).Text(), spans.Eq(1).Text())
		}
	})

	return specs
}

// Ficha de un producto.
type Producto struct {
	Idioma string
	Estado int
	ASIN   string
	TLD    string
	// El formulario de búsqueda es común a todas las páginas y necesita estos
	// dos campos, aunque en una ficha de producto vayan siempre vacíos.
	Sort          string
	Query         string
	Titulo        string
	Imagen        string
	Precio        string
	PrecioTachado string
	Descuento     string
	Valoracion    string
	Resenas       string
	Disponible    string
	Descripcion   []string
	Galeria       []Imagen
	Especs        []Especificacion
	URLAmazon     string
	Error         string
}

// Compone el importe a partir de sus partes. En la ficha de producto el span
// a-offscreen llega vacío, al contrario que en la página de resultados, y el
// importe está repartido entre la parte entera, la fracción y el símbolo.
func precioCompuesto(sel *goquery.Selection) string {
	entero := strings.TrimSpace(sel.Find("span.a-price-whole").First().Text())
	fraccion := strings.TrimSpace(sel.Find("span.a-price-fraction").First().Text())
	simbolo := strings.TrimSpace(sel.Find("span.a-price-symbol").First().Text())
	if entero == "" {
		return ""
	}
	// La parte entera arrastra el separador decimal, que viene en un span
	// anidado dentro de ella.
	separador := ","
	if strings.Contains(entero, ".") {
		separador = "."
	}
	entero = strings.TrimRight(entero, ",.")
	if fraccion == "" {
		return strings.TrimSpace(entero + " " + simbolo)
	}
	return strings.TrimSpace(entero + separador + fraccion + " " + simbolo)
}

// Descripción del producto. Amazon la publica en contenedores distintos según
// la categoría: los artículos corrientes usan una lista de características y
// los libros una reseña editorial.
func descripcionProducto(doc *goquery.Document) []string {
	var lineas []string

	doc.Find("#feature-bullets li span.a-list-item").Each(func(i int, s *goquery.Selection) {
		if t := strings.Join(strings.Fields(s.Text()), " "); t != "" {
			lineas = append(lineas, t)
		}
	})
	if len(lineas) > 0 {
		return lineas
	}

	// En las fichas editoriales cada párrafo lleva un span dentro, de modo que
	// buscar ambos devolvía el mismo texto dos veces. Se recorren solo los
	// párrafos y se descartan los repetidos, porque Amazon publica la misma
	// reseña en más de un contenedor.
	vistas := map[string]bool{}
	for _, contenedor := range []string{"#bookDescription_feature_div", "#productDescription"} {
		doc.Find(contenedor).First().Find("p").Each(func(i int, s *goquery.Selection) {
			t := strings.Join(strings.Fields(s.Text()), " ")
			if len(t) > 40 && !vistas[t] {
				vistas[t] = true
				lineas = append(lineas, t)
			}
		})
		if len(lineas) > 0 {
			return lineas
		}
	}
	return lineas
}

// Construye la URL definitiva de un resultado. Amazon devuelve unas veces una
// ruta relativa y otras una URL absoluta. La plantilla concatenaba el dominio
// sin comprobarlo, lo que en el segundo caso producia enlaces del tipo
// https://amazon.eshttps://www.amazon.es/...
func urlAbsoluta(tld string, enlace string) string {
	if enlace == "" {
		return ""
	}
	if strings.HasPrefix(enlace, "http://") || strings.HasPrefix(enlace, "https://") {
		return enlace
	}
	if !strings.HasPrefix(enlace, "/") {
		enlace = "/" + enlace
	}
	// Se usa el subdominio www porque el dominio desnudo redirige a el, y asi
	// se evita un salto innecesario al pulsar el enlace.
	return "https://www.amazon." + tld + enlace
}

// Extrae el numero de resenas de un aria-label del tipo "5.179 calificaciones"
// o "1,234 ratings". Se recorre solo la parte inicial del texto para no
// arrastrar cifras que aparezcan mas adelante, y se ignoran los separadores de
// millares, que varian segun el marketplace.
func numeroResenas(etiqueta string) (int, bool) {
	var digitos []rune
	for _, r := range strings.TrimSpace(etiqueta) {
		switch {
		case r >= '0' && r <= '9':
			digitos = append(digitos, r)
		case r == '.' || r == ',' || r == '\u00a0' || r == '\u202f':
			// separador de millares
		default:
			if len(digitos) == 0 {
				return 0, false
			}
			n, err := strconv.Atoi(string(digitos))
			return n, err == nil
		}
	}
	if len(digitos) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(string(digitos))
	return n, err == nil
}

func search(ctx context.Context, tld string, searchTerm string, page int, sort string) SearchResults {
	var resultsElement SearchResults
	resultsElement.Query = searchTerm
	resultsElement.TLD = tld
	resultsElement.Page = page
	resultsElement.Sort = sort
	resultsElement.Idioma = idiomaHTML(tld)

	if !tldsPermitidos[tld] {
		resultsElement.Error = "TLD no permitido"
		resultsElement.Estado = http.StatusBadRequest
		return resultsElement
	}

	// Las entradas se guardan y se devuelven sin copiar, asi que a partir de
	// aqui el resultado no debe modificarse: solo se pasa a la plantilla.
	clave := claveCache(tld, searchTerm, page, sort)
	if cacheado, ok := cacheGet(clave); ok {
		return cacheado
	}

	// build the url
	// Se ataca el subdominio www y no el dominio desnudo. Este ultimo es solo
	// un servicio de redireccion que ademas no ofrece HTTP/2: comprobado, el
	// dominio desnudo negocia HTTP/1.1 y www negocia HTTP/2. Eso obligaba a un
	// salto adicional en cada busqueda y degradaba el protocolo.
	requestURL, err := url.Parse("https://www.amazon." + tld + "/s")
	if err != nil {
		resultsElement.Error = "No se pudo construir la URL de busqueda"
		resultsElement.Estado = http.StatusBadRequest
		return resultsElement
	}

	parameters := url.Values{}
	parameters.Add("k", searchTerm)
	parameters.Add("page", strconv.Itoa(page))
	parameters.Add("s", sort)
	requestURL.RawQuery = parameters.Encode()

	// No se fija Accept-Encoding a proposito: cuando la cabecera se establece a
	// mano, el Transport de net/http deja de descomprimir la respuesta de forma
	// transparente y el cuerpo llegaria comprimido al parser.
	cabeceras := [][2]string{
		{"User-Agent", userAgent},
		{"Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		{"Upgrade-Insecure-Requests", "1"},
	}
	if idioma, ok := idiomaPorTLD[tld]; ok {
		cabeceras = append(cabeceras, [2]string{"Accept-Language", idioma})
	}

	res, err := obtener(ctx, requestURL.String(), cabeceras, maxCuerpo)
	if err != nil {
		fmt.Println("fallo al contactar con Amazon:", err)
		resultsElement.Error = "No se pudo contactar con Amazon"
		resultsElement.Estado = http.StatusBadGateway
		return resultsElement
	}

	if res.Estado != 200 {
		fmt.Println("Amazon respondio con estado:", res.Estado)
		resultsElement.Error = "Amazon ha respondido con un error"
		resultsElement.Estado = http.StatusBadGateway
		return resultsElement
	}

	// El cuerpo ya viene leido y acotado: las paginas de verificacion llegan
	// con codigo 200 y hay que inspeccionarlo antes de parsearlo, porque de lo
	// contrario se interpretarian como una busqueda sin resultados.
	cuerpo := res.Cuerpo
	if err != nil {
		fmt.Println("fallo al leer la respuesta de Amazon:", err)
		resultsElement.Error = "No se pudo leer la respuesta de Amazon"
		resultsElement.Estado = http.StatusBadGateway
		return resultsElement
	}

	if esBloqueo(cuerpo) {
		// Se registra el detalle de la respuesta bloqueada. Sin estos datos no
		// hay forma de saber por que Amazon rechaza una peticion concreta:
		// desde fuera, un bloqueo es indistinguible de otro.
		fmt.Printf("bloqueo antibot: tld=%s protocolo=%s bytes=%d server=%q via=%q rid=%q\n",
			tld, res.Proto, len(cuerpo),
			res.Cabecera("Server"),
			res.Cabecera("Via"),
			res.Cabecera("X-Amz-Rid"))
		resultsElement.Error = "Amazon ha respondido con una verificacion antibot en lugar de resultados. Intentalo de nuevo en unos minutos."
		resultsElement.Estado = http.StatusServiceUnavailable
		return resultsElement
	}

	// Load the HTML document
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(cuerpo))
	if err != nil {
		fmt.Println("fallo al parsear el HTML de Amazon:", err)
		resultsElement.Error = "No se pudo interpretar la respuesta de Amazon"
		resultsElement.Estado = http.StatusBadGateway
		return resultsElement
	}

	// Numero de paginas de resultados. Se recorre con Find y no con
	// ChildrenFiltered porque los elementos de paginacion no son hijos directos
	// del contenedor: cuelgan de una lista intermedia, en la mayoria de los
	// casos a traves de un li y un span. Los que no llevan numero (flechas de
	// avance y retroceso, puntos suspensivos) los descarta la conversion.
	largestPage := 1
	doc.Find("span.s-pagination-strip").Find(".s-pagination-item").Each(func(i int, child *goquery.Selection) {
		page, _ := strconv.Atoi(strings.TrimSpace(child.Text()))
		if page > largestPage {
			largestPage = page
		}
	})
	resultsElement.Pages = largestPage

	// find the search results
	var searchResults []SearchResult

	doc.Find(".s-result-item").Each(func(i int, result *goquery.Selection) {
		// El titulo va en un h2 dentro del contenedor marcado con
		// data-cy="title-recipe". Se usa el atributo en lugar de la clase
		// s-title-instructions-style porque las clases utilitarias de Amazon
		// rotan con mas frecuencia.
		titleEl := result.Find("[data-cy='title-recipe'] h2").First()
		title := titleEl.Text()

		// El enlace es el <a> que envuelve al h2. En una minoria de tarjetas
		// Amazon sigue sirviendo la estructura antigua, con el <a> dentro del
		// h2; Closest devuelve vacio en ese caso y se recurre a Find.
		linkEl := titleEl.Closest("a")
		if linkEl.Length() == 0 {
			linkEl = titleEl.Find("a").First()
		}
		link, _ := linkEl.Attr("href")
		// Se elimina la parte de seguimiento del enlace. La variable se llama
		// enlace y no url para no ocultar el paquete del mismo nombre.
		enlace, err := url.Parse(link)
		if err == nil {
			dontParse := false
			// Los resultados patrocinados apuntan a un redirector que lleva el
			// producto real en el parametro url. Amazon usa dos formatos:
			// /gp/slredirect, el antiguo, y /sspa/click, el actual. Este ultimo
			// no se contemplaba, de modo que al vaciar el query string se
			// descartaba con el el producto y el enlace acababa apuntando al
			// propio redirector.
			if strings.HasPrefix(enlace.Path, "/gp/") || strings.HasPrefix(enlace.Path, "/sspa/") {
				if val, ok := enlace.Query()["url"]; ok && val[0] != "" {
					// El valor es una ruta con su propio query string, asi que
					// se parsea en lugar de asignarse tal cual a Path.
					if interno, errInterno := url.Parse(val[0]); errInterno == nil {
						enlace = interno
					} else {
						dontParse = true
					}
				} else {
					dontParse = true
				}
			}

			if !dontParse {
				enlace.RawQuery = ""

				pathParts := strings.Split(enlace.Path, "/")
				if strings.HasPrefix(pathParts[len(pathParts)-1], "ref") {
					enlace.Path = strings.Join(pathParts[:len(pathParts)-1], "/")
				}

				link = enlace.String()
			}
		}

		// De entre los formatos disponibles se muestra el mas barato de pago,
		// indicando a cual corresponde. Si el unico importe es cero se muestra
		// igualmente, acompanado del precio tachado y de la condicion de
		// acceso, para que quede claro que no es un producto gratuito.
		price := ""
		type_ := ""
		pares := formatosYPrecios(result)
		mejor := -1
		primerCero := -1
		for j, p := range pares {
			valor, ok := importePrecio(p.Precio)
			if !ok {
				continue
			}
			if valor <= 0 {
				if primerCero < 0 {
					primerCero = j
				}
				continue
			}
			if mejor < 0 {
				mejor = j
				continue
			}
			if anterior, _ := importePrecio(pares[mejor].Precio); valor < anterior {
				mejor = j
			}
		}
		if mejor < 0 {
			mejor = primerCero
		}
		if mejor >= 0 {
			price = pares[mejor].Precio
			type_ = pares[mejor].Formato
		}

		imagenSrc, _ := result.Find("img.s-image").First().Attr("src")
		image := rutaProxyImagen(imagenSrc)

		// Valoracion y numero de resenas. Se anclan al atributo data-cy en vez
		// de a la cadena de clases utilitarias, y se leen de donde Amazon los
		// expone hoy: la puntuacion en el texto alternativo del icono de
		// estrellas, y el numero exacto en el aria-label del enlace a las
		// opiniones. El texto visible de ese enlace llega abreviado, del tipo
		// "(9,2 mil)", y no sirve como fuente.
		bloqueResenas := result.Find("[data-cy='reviews-block']").First()
		ratings := strings.TrimSpace(bloqueResenas.Find("span.a-icon-alt").First().Text())

		reviews := 0
		etiquetaResenas, _ := bloqueResenas.Find("a[href*='customerReviews']").First().Attr("aria-label")
		if n, ok := numeroResenas(etiquetaResenas); ok {
			reviews = n
		} else if etiquetaResenas != "" {
			fmt.Println("no se pudo leer el numero de resenas de:", etiquetaResenas)
		}

		limitedSupplyEl := result.Find("div.sg-col-inner > div.a-section.a-spacing-none.a-spacing-top-micro > div").Last()
		limitedSupply := limitedSupplyEl.Find("span.a-size-base.a-color-price").Text()

		// La URL se deja construida aquí, no en la plantilla, para que esta no
		// tenga que saber si el enlace venía relativo o absoluto.
		link = urlAbsoluta(tld, link)

		// Si el enlace apunta a una ficha de producto, se reescribe hacia la
		// del propio frontend para no enviar al usuario a Amazon.
		if asin := asinDeURL(link); asin != "" {
			link = "/dp/" + asin + "?tld=" + tld
		}

		// Sin enlace no hay resultado que ofrecer: antes se emitia un enlace a
		// la portada de Amazon, que no lleva al producto.
		if strings.Trim(title, " ") != "" && link != "" {
			var res SearchResult

			res.Title = title
			res.URL = link
			res.Price = price
			res.PrecioTachado = precioTachado(result)
			res.Condicion = condicionAcceso(result)
			res.ImageURL = image
			res.Type = type_
			res.Reviews = reviews
			res.Ratings = ratings
			res.LimitedSupply = limitedSupply

			searchResults = append(searchResults, res)
		}
	})

	resultsElement.Results = searchResults

	// Solo se cachean las busquedas correctas: un error o un bloqueo no debe
	// quedarse fijado durante todo el TTL.
	cacheSet(clave, resultsElement)

	return resultsElement
}

// Obtiene la ficha de un producto. Reutiliza el cliente saliente, la detección
// de bloqueo y la caché de la búsqueda; solo cambia lo que se extrae.
func producto(ctx context.Context, tld string, asin string) Producto {
	var ficha Producto
	ficha.ASIN = asin
	ficha.TLD = tld
	ficha.Idioma = idiomaHTML(tld)

	if !tldsPermitidos[tld] {
		ficha.Error = "TLD no permitido"
		ficha.Estado = http.StatusBadRequest
		return ficha
	}
	if !asinValido(asin) {
		ficha.Error = "Identificador de producto no válido"
		ficha.Estado = http.StatusBadRequest
		return ficha
	}

	ficha.URLAmazon = "https://www.amazon." + tld + "/dp/" + asin

	clave := claveCache(tld, "dp:"+asin, 0, "")
	if cacheado, ok := cacheProducto(clave); ok {
		return cacheado
	}

	cabeceras := [][2]string{
		{"User-Agent", userAgent},
		{"Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		{"Upgrade-Insecure-Requests", "1"},
	}
	if idioma, ok := idiomaPorTLD[tld]; ok {
		cabeceras = append(cabeceras, [2]string{"Accept-Language", idioma})
	}

	res, err := obtener(ctx, ficha.URLAmazon, cabeceras, maxCuerpo)
	if err != nil {
		fmt.Println("fallo al contactar con Amazon:", err)
		ficha.Error = "No se pudo contactar con Amazon"
		ficha.Estado = http.StatusBadGateway
		return ficha
	}
	if res.Estado != 200 {
		fmt.Println("Amazon respondió con estado:", res.Estado)
		ficha.Error = "Amazon ha respondido con un error"
		ficha.Estado = http.StatusBadGateway
		return ficha
	}
	if esBloqueo(res.Cuerpo) {
		fmt.Printf("bloqueo antibot: tld=%s asin=%s protocolo=%s bytes=%d server=%q\n",
			tld, asin, res.Proto, len(res.Cuerpo), res.Cabecera("Server"))
		ficha.Error = "Amazon ha respondido con una verificación antibot en lugar del producto. Inténtalo de nuevo en unos minutos."
		ficha.Estado = http.StatusServiceUnavailable
		return ficha
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(res.Cuerpo))
	if err != nil {
		fmt.Println("fallo al parsear el HTML de Amazon:", err)
		ficha.Error = "No se pudo interpretar la respuesta de Amazon"
		ficha.Estado = http.StatusBadGateway
		return ficha
	}

	ficha.Titulo = strings.Join(strings.Fields(doc.Find("#productTitle").First().Text()), " ")
	if ficha.Titulo == "" {
		ficha.Error = "No se encontró el producto"
		ficha.Estado = http.StatusNotFound
		return ficha
	}

	if src, ok := doc.Find("#landingImage").First().Attr("src"); ok {
		ficha.Imagen = rutaProxyImagen(src)
	}

	ficha.Precio = precioCompuesto(doc.Find("span.priceToPay").First())
	ficha.PrecioTachado = strings.TrimSpace(
		doc.Find("span.apex-basisprice-offscreen-label").First().Text())
	ficha.Descuento = strings.TrimSpace(doc.Find("span.savingsPercentage").First().Text())

	ficha.Valoracion, _ = doc.Find("#acrPopover").First().Attr("title")
	ficha.Resenas, _ = doc.Find("#acrCustomerReviewText").First().Attr("aria-label")
	ficha.Disponible = strings.Join(strings.Fields(
		doc.Find("#availability span").First().Text()), " ")
	ficha.Descripcion = descripcionProducto(doc)
	ficha.Galeria = galeria(res.Cuerpo)
	ficha.Especs = especificaciones(doc)

	cacheSetProducto(clave, ficha)
	return ficha
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	tld := "com"
	query := ""
	page := 1
	sort := ""

	if r.Method == "GET" {
		// get url parameters

		tldInQuery := r.URL.Query()["tld"]
		if len(tldInQuery) > 0 {
			tld = tldInQuery[0]
		}

		queryInQuery := r.URL.Query()["k"]
		if len(queryInQuery) > 0 {
			query = queryInQuery[0]
		}

		sortInQuery := r.URL.Query()["s"]
		if len(sortInQuery) > 0 {
			sort = sortInQuery[0]
		}

		pageInQuery := r.URL.Query()["page"]
		if len(pageInQuery) > 0 {
			convPage, err := strconv.Atoi(pageInQuery[0])

			if err != nil {
				// we couldn't parse the page argument, so use the default page
				page = 1
			} else {
				page = convPage
			}
		}

	} else if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			fmt.Println("fallo al parsear el formulario:", err)
			http.Error(w, "No se pudo procesar el formulario", http.StatusBadRequest)
			return
		}

		// build the url
		searchURL, _ := url.Parse("/s")

		pageFromValue, _ := strconv.Atoi(r.FormValue("page"))
		page := int(math.Max(float64(pageFromValue), 1))

		parameters := url.Values{}
		parameters.Add("tld", r.FormValue("tld"))
		parameters.Add("k", r.FormValue("k"))
		parameters.Add("s", r.FormValue("s"))
		parameters.Add("page", strconv.Itoa(page))
		searchURL.RawQuery = parameters.Encode()

		// Sin este return la ejecucion continuaba hacia el cuerpo comun de la
		// funcion, que hacia una busqueda completa contra Amazon con los
		// valores por defecto y escribia sobre una respuesta ya enviada.
		http.Redirect(w, r, searchURL.String(), http.StatusSeeOther)
		return
	} else {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Solo se admiten los metodos GET y POST", http.StatusMethodNotAllowed)
		return
	}

	// Sin termino de busqueda no hay nada que consultar. Antes la variable se
	// inicializaba con una cadena de relleno que se acababa buscando de verdad
	// en Amazon, gastando una peticion saliente a cambio de nada.
	if strings.TrimSpace(query) == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if !ordenesPermitidos[sort] {
		http.Error(w, "Criterio de ordenacion no admitido", http.StatusBadRequest)
		return
	}

	// El numero de pagina se reenviaba tal cual a Amazon, incluidos negativos y
	// valores arbitrariamente grandes.
	if page < 1 || page > paginaMaxima {
		http.Error(w, "Numero de pagina fuera de rango", http.StatusBadRequest)
		return
	}

	// El TLD se comprueba también aquí, y no solo en search, para no llegar a
	// almacenar en una cookie un valor que después será rechazado.
	if !tldsPermitidos[tld] {
		http.Error(w, "TLD no permitido", http.StatusBadRequest)
		return
	}

	// Se recuerdan el dominio y el criterio de ordenación elegidos. Ambos
	// valores están ya validados contra sus listas cerradas en este punto.
	//
	// Antes se guardaban sin ningún atributo: sin Path, de modo que el ámbito
	// dependía de la ruta de la petición; sin HttpOnly, accesibles desde
	// cualquier script de la página; sin SameSite, enviadas en peticiones de
	// terceros; y sin Max-Age, con lo que la preferencia se perdía al cerrar el
	// navegador, justo lo contrario de lo que la funcionalidad pretende.
	//
	// Secure se activa solo cuando la conexión del usuario es cifrada. La
	// aplicación suele ejecutarse detrás de un proxy inverso que termina el
	// TLS y reenvía en claro, de modo que se consulta también la cabecera que
	// ese proxy añade.
	segura := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"

	guardarPreferencia := func(nombre, valor string) {
		http.SetCookie(w, &http.Cookie{
			Name:     nombre,
			Value:    valor,
			Path:     "/",
			HttpOnly: true,
			Secure:   segura,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   60 * 60 * 24 * 365,
		})
	}

	guardarPreferencia("SortingCookie", sort)
	guardarPreferencia("TLDCookie", tld)

	// get the result

	result := search(r.Context(), tld, query, page, sort)

	if result.Error != "" {
		// El mensaje se renderiza dentro de la plantilla y con un codigo de
		// estado propio. Antes se escribia en el cuerpo pasandolo como cadena
		// de formato a Fprintf, lo que corrompia la salida cuando contenia
		// secuencias como %3E procedentes de una URL.
		estado := result.Estado
		if estado == 0 {
			estado = http.StatusBadGateway
		}
		w.WriteHeader(estado)
		if err := plantillaError.ExecuteTemplate(w, "base.html", result); err != nil {
			fmt.Println("error al renderizar la pagina de error:", err)
		}
		return
	}

	if err := plantillaResultados.ExecuteTemplate(w, "base.html", result); err != nil {
		// La cabecera ya se envio, de modo que solo cabe dejar constancia.
		fmt.Println("error al renderizar los resultados:", err)
	}
}

// Sirve la ficha de un producto en la ruta /dp/{asin}.
func handleProducto(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Solo se admite el método GET", http.StatusMethodNotAllowed)
		return
	}

	asin := strings.Trim(strings.TrimPrefix(r.URL.Path, "/dp/"), "/")

	// El dominio se toma de la preferencia guardada y puede sobreescribirse
	// por parámetro, que es lo que hacen los enlaces de los resultados.
	tld := "com"
	if c, err := r.Cookie("TLDCookie"); err == nil && tldsPermitidos[c.Value] {
		tld = c.Value
	}
	if v := r.URL.Query().Get("tld"); v != "" {
		tld = v
	}

	ficha := producto(r.Context(), tld, asin)

	if ficha.Error != "" {
		estado := ficha.Estado
		if estado == 0 {
			estado = http.StatusBadGateway
		}
		w.WriteHeader(estado)
		if err := plantillaError.ExecuteTemplate(w, "base.html", ficha); err != nil {
			fmt.Println("error al renderizar la página de error:", err)
		}
		return
	}

	if err := plantillaProducto.ExecuteTemplate(w, "base.html", ficha); err != nil {
		fmt.Println("error al renderizar el producto:", err)
	}
}

func proxyMedia(w http.ResponseWriter, r *http.Request) {
	// La ruta tiene la forma /mediaproxy/{host}/{recurso}. El host se valida
	// contra la lista de CDN conocidos antes de construir la peticion.
	resto := strings.TrimPrefix(r.URL.Path, "/mediaproxy/")
	host, recurso, ok := strings.Cut(resto, "/")
	if !ok || !cdnsImagen[host] {
		http.Error(w, "Origen de imagen no permitido", http.StatusForbidden)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), "GET", "https://"+host+"/"+recurso, nil)
	if err != nil {
		http.Error(w, "Peticion de imagen invalida", http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/avif,image/webp,*/*")

	resp, err := clienteHTTP.Do(req)
	if err != nil {
		http.Error(w, "No se pudo obtener la imagen", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	// Sin esta comprobacion, un 404 o un 403 del upstream se reenviaba como un
	// 200 con el cuerpo del error dentro.
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "La imagen no esta disponible", http.StatusBadGateway)
		return
	}

	// Solo se reenvian imagenes. Sin esta comprobacion el navegador aplicaria
	// su propia deteccion de tipo sobre un contenido que no controlamos.
	tipo := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(tipo, "image/") {
		http.Error(w, "El recurso solicitado no es una imagen", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", tipo)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Las URL de imagen de Amazon incluyen el identificador del recurso, asi
	// que su contenido no cambia. Cachearlas evita repetir la peticion saliente
	// en cada carga de pagina.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if resp.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}

	if _, err := io.Copy(w, io.LimitReader(resp.Body, maxImagen)); err != nil {
		// La cabecera ya se envio, de modo que solo cabe dejar constancia.
		fmt.Println("error al reenviar la imagen:", err)
	}
}

func main() {
	// El valor por defecto del host es 0.0.0.0 y no localhost. Dentro de un
	// contenedor, localhost bindea al bucle interno y deja el proceso
	// inalcanzable desde fuera aunque el puerto esté publicado.
	puertoPorDefecto, err := strconv.Atoi(entorno("SIMPLEAMAZON_PORT", "8080"))
	if err != nil {
		log.Fatalf("SIMPLEAMAZON_PORT no es un número válido: %v", err)
	}

	port := flag.Int("p", puertoPorDefecto, "Puerto de escucha")
	host := flag.String("h", entorno("SIMPLEAMAZON_HOST", "0.0.0.0"), "Dirección de escucha")
	huella := flag.Bool("huella", false, "Consulta la huella TLS y HTTP/2 del cliente, la muestra y termina")
	perfil := flag.String("tls", "go", "Huella de las peticiones salientes: go, navegador o curl")
	volcado := flag.Bool("volcado", false, "Imprime por consola las peticiones salientes y sus respuestas")
	flag.Parse()

	// El cliente estandar sigue siendo el predeterminado. El perfil de
	// navegador se activa de forma explicita, de modo que se puede volver al
	// comportamiento anterior sin recompilar.
	switch *perfil {
	case "navegador":
		sesionNavegador = azuretls.NewSession()
		sesionNavegador.Browser = azuretls.Firefox
		fmt.Println("huella: imitando navegador (TLS y HTTP/2)")

	case "curl":
		// Perfil medido de un curl que si atraviesa el servicio antibot desde
		// esta misma maquina. No se parece a ningun navegador, de modo que si
		// funciona quedara descartado que el criterio sea parecerse a uno.
		// El separador de los ajustes es una coma y no el punto y coma que usan
		// las herramientas de inspeccion de huella.
		sesionNavegador = azuretls.NewSession()
		if err := sesionNavegador.ApplyJa3(ja3Curl, azuretls.Firefox); err != nil {
			fmt.Println("no se pudo aplicar el JA3:", err)
			os.Exit(1)
		}
		if err := sesionNavegador.ApplyHTTP2(akamaiCurl); err != nil {
			fmt.Println("no se pudo aplicar la huella HTTP/2:", err)
			os.Exit(1)
		}
		fmt.Println("huella: imitando curl (TLS y HTTP/2)")
	}

	// El volcado imprime por consola la peticion saliente completa, con sus
	// cabeceras y el orden en que se envian, y la respuesta recibida. Sirve
	// para comparar lo que el programa envia de verdad con lo que envia un
	// cliente que si atraviesa el servicio antibot, en lugar de deducirlo.
	if *volcado && sesionNavegador != nil {
		sesionNavegador.Log()
		fmt.Println("volcado de peticiones activado")
	}

	if *huella {
		imprimirHuella()
		return
	}

	fmt.Printf("Serving on %s:%d\n", *host, *port)
	// Se usa un multiplexor propio en lugar del global por defecto, para que
	// las rutas queden acotadas a este servidor.
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var val TemplateValues
		// read the cookie
		sortCookie, err := r.Cookie("SortingCookie")
		if err == nil {
			val.Sort = sortCookie.Value
		}

		tldCookie, err := r.Cookie("TLDCookie")
		if err == nil {
			val.TLD = tldCookie.Value
		} else {
			val.TLD = "com"
		}
		val.Idioma = idiomaHTML(val.TLD)

		if r.URL.Path != "/" {
			// Antes cualquier ruta desconocida respondia 200, de modo que no
			// habia forma de distinguir una direccion valida de una que no
			// existe. La plantilla se conserva como cuerpo de la respuesta.
			w.WriteHeader(http.StatusNotFound)
			err = plantillaUnhandled.ExecuteTemplate(w, "base.html", val)
		} else {
			err = plantillaIndex.ExecuteTemplate(w, "base.html", val)
		}

		if err != nil {
			// La cabecera ya se envio, asi que escribir el error en el cuerpo
			// solo produciria una pagina a medias.
			fmt.Println("error al renderizar la plantilla:", err)
		}
	})

	// Los navegadores piden /favicon.ico por convención, sin leer el head, así
	// que la ruta se registra además de referenciarlo desde la plantilla. El
	// icono no cambia, de modo que se sirve con una validez larga.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		icono, err := staticFiles.ReadFile("static/favicon.ico")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=604800")
		if _, err := w.Write(icono); err != nil {
			fmt.Println("error al servir el favicon:", err)
		}
	})

	// Sin robots.txt, los buscadores pueden indexar la instancia, y cada visita
	// indexada se traduce en trafico saliente hacia Amazon que no controlamos.
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
	})

	mux.HandleFunc("/s", handleSearch)
	mux.HandleFunc("/dp/", handleProducto)
	mux.HandleFunc("/mediaproxy/", proxyMedia)

	mux.Handle("/static/", http.FileServer(http.FS(staticFiles)))

	// ListenAndServe usa un servidor sin ningun timeout, vulnerable a que un
	// cliente lento mantenga conexiones abiertas indefinidamente.
	srv := &http.Server{
		Addr:              *host + ":" + strconv.Itoa(*port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	// El error de arranque se trataba con Println, de modo que un fallo al
	// enlazar el puerto se imprimía y el proceso terminaba con código cero, lo
	// que un supervisor interpreta como una salida limpia.
	log.Fatal(srv.ListenAndServe())
}
