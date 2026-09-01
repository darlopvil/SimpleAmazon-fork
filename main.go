package main
import ( "fmt"
    "strings"
    "strconv"
    "net/url"
    "context"
    "bytes"
    "sync"
    "net/http"
    "html/template"
    "math"
    "io"
    "flag"
    "embed"
    "time"
    
    "github.com/PuerkitoBio/goquery"
)

var (
    //go:embed templates/index.html
    indexTemplate string
    //go:embed templates/searchResults.html
    searchResultsTemplate string
    //go:embed templates/unhandled.html
    unhandledTemplate string

    //go:embed static
    staticFiles embed.FS
)

type SearchResult struct {
    Title string
    URL string
    Price string
    ImageURL string
    Type string
    Ratings string
    Reviews int
    LimitedSupply string
}

type SearchResults struct {
    Pages int
    Page int
    TLD string
    Sort string
    Query string
    Results []SearchResult
    Error string
}

type TemplateValues struct {
    Sort string
    TLD string
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
    plantillaIndex     = template.Must(template.New("index").Parse(indexTemplate))
    plantillaUnhandled = template.Must(template.New("unhandled").Parse(unhandledTemplate))
    plantillaResultados = template.Must(template.New("searchResults.html").Funcs(template.FuncMap{
        "substract": func(a, b int) int {
            return a - b
        },
        "add": func(a, b int) int {
            return a + b
        },
    }).Parse(searchResultsTemplate))
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

    if !tldsPermitidos[tld] {
        resultsElement.Error = "TLD no permitido"
        return resultsElement
    }

    // Las entradas se guardan y se devuelven sin copiar, asi que a partir de
    // aqui el resultado no debe modificarse: solo se pasa a la plantilla.
    clave := claveCache(tld, searchTerm, page, sort)
    if cacheado, ok := cacheGet(clave); ok {
        return cacheado
    }

    // build the url
    requestURL, err := url.Parse("https://amazon." + tld + "/s")
    if err != nil {
        resultsElement.Error = fmt.Sprintf("Invalid URL https://amazon.%s/s", tld)
        return resultsElement
    }

    parameters := url.Values{}
    parameters.Add("k", searchTerm)
    parameters.Add("page", strconv.Itoa(page))
    parameters.Add("s", sort)
    requestURL.RawQuery = parameters.Encode()


    // Request the page
    req, err := http.NewRequestWithContext(ctx, "GET", requestURL.String(), nil)
    if err != nil {
        resultsElement.Error = fmt.Sprintf("Error creating request %s", err)
        return resultsElement
    }

    // No se fija Accept-Encoding a proposito: cuando la cabecera se establece a
    // mano, el Transport de net/http deja de descomprimir la respuesta de forma
    // transparente y el cuerpo llegaria comprimido al parser.
    req.Header.Set("User-Agent", userAgent)
    req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
    req.Header.Set("Upgrade-Insecure-Requests", "1")
    if idioma, ok := idiomaPorTLD[tld]; ok {
        req.Header.Set("Accept-Language", idioma)
    }

    res, err := clienteHTTP.Do(req)
    if err != nil {
        resultsElement.Error = fmt.Sprintf("Couldn't fetch search result: %s", err)
        return resultsElement
    }
    defer res.Body.Close()

    if res.StatusCode != 200 {
        resultsElement.Error = fmt.Sprintf("Status code error: %d %s", res.StatusCode, res.Status)
        return resultsElement
    }

    // El cuerpo se lee a memoria para poder inspeccionarlo antes de parsearlo:
    // las paginas de verificacion llegan con codigo 200 y sin ellas se
    // interpretarian como una busqueda sin resultados.
    cuerpo, err := io.ReadAll(io.LimitReader(res.Body, maxCuerpo))
    if err != nil {
        resultsElement.Error = fmt.Sprintf("Couldn't read search result: %s", err)
        return resultsElement
    }

    if esBloqueo(cuerpo) {
        resultsElement.Error = "Amazon ha respondido con una verificacion antibot en lugar de resultados. Intentalo de nuevo en unos minutos."
        return resultsElement
    }

    // Load the HTML document
    doc, err := goquery.NewDocumentFromReader(bytes.NewReader(cuerpo))
    if err != nil {
        resultsElement.Error = fmt.Sprintf("Couldn't parse HTML result: %s", err)
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

        price := result.Find("span.a-price > span.a-offscreen").First().Text()
        type_ := result.Find("a.a-size-base.a-link-normal.s-underline-text.s-underline-link-text.s-link-style.a-text-bold").Text()

        image, _ := result.Find("img.s-image").First().Attr("src")
        // length of https://m.media-amazon.com : 26
        if len(image) > 26 {
            image = "/mediaproxy" + image[26:]
        }

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


        // La URL se deja construida aqui, no en la plantilla, para que esta no
        // tenga que saber si el enlace venia relativo o absoluto.
        link = urlAbsoluta(tld, link)

        // Sin enlace no hay resultado que ofrecer: antes se emitia un enlace a
        // la portada de Amazon, que no lleva al producto.
        if strings.Trim(title, " ") != "" && link != "" {
            var res SearchResult

            res.Title = title
            res.URL = link
            res.Price = price
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

func handleSearch(w http.ResponseWriter, r *http.Request) {
    tld := "com"
    query := "default query, should never be shown."
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
            fmt.Fprintf(w, "Parseform() err %s", err)
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


    // save a cookie containing sorting and TLD (YAY! Session Management)
    http.SetCookie(w, &http.Cookie{
        Name: "SortingCookie",
        Value: sort,
    })

    http.SetCookie(w, &http.Cookie{
        Name: "TLDCookie",
        Value: tld,
    })


    // get the result

    result := search(r.Context(), tld, query, page, sort)

    if result.Error != "" {
        fmt.Fprintf(w, result.Error)
        return
    }

    if err := plantillaResultados.Execute(w, result); err != nil {
        // La cabecera ya se envio, de modo que solo cabe dejar constancia.
        fmt.Println("error al renderizar los resultados:", err)
    }
}

func proxyMedia(w http.ResponseWriter, r *http.Request) {
    // len of /mediaproxy: 12
    url := r.URL.Path[12:]

    req, err := http.NewRequestWithContext(r.Context(), "GET", "https://m.media-amazon.com/"+url, nil)
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
    port := flag.Int("p", 8080, "Port")
    host := flag.String("h", "localhost", "Host")
    flag.Parse()

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


        if r.URL.Path != "/" {
            // Antes cualquier ruta desconocida respondia 200, de modo que no
            // habia forma de distinguir una direccion valida de una que no
            // existe. La plantilla se conserva como cuerpo de la respuesta.
            w.WriteHeader(http.StatusNotFound)
            err = plantillaUnhandled.Execute(w, val)
        } else {
            err = plantillaIndex.Execute(w, val)
        }

        if err != nil {
            // La cabecera ya se envio, asi que escribir el error en el cuerpo
            // solo produciria una pagina a medias.
            fmt.Println("error al renderizar la plantilla:", err)
        }
    })

    // Sin robots.txt, los buscadores pueden indexar la instancia, y cada visita
    // indexada se traduce en trafico saliente hacia Amazon que no controlamos.
    mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
    })

    mux.HandleFunc("/s", handleSearch)
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
    fmt.Println(srv.ListenAndServe())
}
