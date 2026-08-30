package main
import ( "fmt"
    "strings"
    "strconv"
    "net/url"
    "net/http"
    "html/template"
    "math"
    "io"
    "flag"
    "embed"
    
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

func search(tld string, searchTerm string, page int, sort string) SearchResults {
    var resultsElement SearchResults
    resultsElement.Query = searchTerm
    resultsElement.TLD = tld
    resultsElement.Page = page
    resultsElement.Sort = sort

    if !tldsPermitidos[tld] {
        resultsElement.Error = "TLD no permitido"
        return resultsElement
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
    client := &http.Client{}
    req, err := http.NewRequest("GET", requestURL.String(), nil)
    if err != nil {
        resultsElement.Error = fmt.Sprintf("Error creating request %s", err)
        return resultsElement
    }

    req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 6.1; Win64; x64; rv:47.0) Gecko/20100101 Firefox/47.0")

    res, err := client.Do(req)
    if err != nil {
        resultsElement.Error = fmt.Sprintf("Couldn't fetch search result: %s", err)
        return resultsElement
    }
    defer res.Body.Close()

    if res.StatusCode != 200 {
        resultsElement.Error = fmt.Sprintf("Status code error: %d %s", res.StatusCode, res.Status)
        return resultsElement
    }

    // Load the HTML document
    doc, err := goquery.NewDocumentFromReader(res.Body)
    if err != nil {
        resultsElement.Error = fmt.Sprintf("Couldn't parse HTML result: %s", err)
        return resultsElement
    }

    //find out how many pages of search results there are
    //sadly amazon's pagination is inconsistent, so we need to do this to get a consistent result
    largestPage := 1
    doc.Find("span.s-pagination-strip").ChildrenFiltered(".s-pagination-item").Each(func(i int, child *goquery.Selection) {
        page, _ := strconv.Atoi(child.Text())
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
        // Strip the tracking parts of the URL if possible
        url, err := url.Parse(link)
        if err == nil {
            dontParse := false
            if strings.HasPrefix(url.Path, "/gp/") {
                // the URL query parameter contains the actual name of the URL, so we can save ourselves a trip to the amazon redirect domain and just get the URL and then let it run through the parser down below
                if val, ok := url.Query()["url"]; ok {
                    url.Path = val[0]
                } else {
                    dontParse = true
                }
            }

            if !dontParse {
                url.RawQuery = ""

                pathParts := strings.Split(url.Path, "/")
                if strings.HasPrefix(pathParts[len(pathParts) - 1], "ref") {
                    url.Path = strings.Join(pathParts[:len(pathParts) - 1], "/")
                }

                link = url.String()
            }
        }

        price := result.Find("span.a-price > span.a-offscreen").First().Text()
        type_ := result.Find("a.a-size-base.a-link-normal.s-underline-text.s-underline-link-text.s-link-style.a-text-bold").Text()

        image, _ := result.Find("img.s-image").First().Attr("src")
        // length of https://m.media-amazon.com : 26
        if len(image) > 26 {
            image = "/mediaproxy" + image[26:]
        }

        reviewsAndRatingsEl := result.Find("div.a-row.a-size-small").First()
        reviews, _ := strconv.Atoi(reviewsAndRatingsEl.Find("span").Last().Text())
        ratings := reviewsAndRatingsEl.Find("span").First().Find("span").Last().Text()

        limitedSupplyEl := result.Find("div.sg-col-inner > div.a-section.a-spacing-none.a-spacing-top-micro > div").Last()
        limitedSupply := limitedSupplyEl.Find("span.a-size-base.a-color-price").Text()


        if strings.Trim(title, " ") != "" {
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

        http.Redirect(w, r, searchURL.String(), http.StatusSeeOther)
    } else {
        fmt.Fprintf(w, "Sorry, only POST or GET is supported.")
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

    result := search(tld, query, page, sort)

    if result.Error != "" {
        fmt.Fprintf(w, result.Error)
        return
    }

    // expose simple add / subtract functions to the template
    fm := template.FuncMap{
        "substract": func(a, b int) int {
            return a - b
        },
        "add": func(a, b int) int {
            return a + b
        },
    }

    t, err := template.New("searchResults.html").Funcs(fm).Parse(searchResultsTemplate)
    if err != nil {
        fmt.Println(err)
    }
    err = t.Execute(w, result)

    if err != nil {
        fmt.Println(err)
    }
}

func proxyMedia(w http.ResponseWriter, r *http.Request) {
    // len of /mediaproxy: 12
    url := r.URL.Path[12:]

    resp, err := http.Get("https://m.media-amazon.com/" + url)
    if err != nil {
        http.Error(w, err.Error(), http.StatusServiceUnavailable)
        return
    }
    defer resp.Body.Close()
    io.Copy(w, resp.Body)
}

func main() {
    indexTempl, err := template.New("index").Parse(indexTemplate)
    if err != nil {
        panic(err)
    }
    unhandledTempl, err := template.New("unhandled").Parse(unhandledTemplate)
    if err != nil {
        panic(err)
    }

    port := flag.Int("p", 8080, "Port")
    host := flag.String("h", "localhost", "Host")
    flag.Parse()

    fmt.Printf("Serving on %s:%d\n", *host, *port)
    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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


        if len(r.URL.Path) > 1 {
            err = unhandledTempl.Execute(w, val)
        } else {
            err = indexTempl.Execute(w, val)
        }

        if err != nil {
            fmt.Fprintf(w, err.Error())
        }
    })

    http.HandleFunc("/s", handleSearch)
    http.HandleFunc("/mediaproxy/", proxyMedia)

    http.Handle("/static/", http.FileServer(http.FS(staticFiles)))

    fmt.Println(http.ListenAndServe(*host + ":" + strconv.Itoa(*port), nil))
}
