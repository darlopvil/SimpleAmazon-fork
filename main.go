package main
import ( "fmt"
    "log"
    "strings"
    "strconv"
    "net/url"
    "net/http"
    "html/template"
    "math"
    "io"
    "flag"
    _ "embed"
    
    "github.com/PuerkitoBio/goquery"
)

var (
    //go:embed templates/index.html
    indexTemplate string
    //go:embed templates/searchResults.html
    searchResultsTemplate string
    //go:embed templates/unhandled.html
    unhandledTemplate string
)

type SearchResult struct {
    Title string
    URL string
    Price string
    ImageURL string
    Type string
}

type SearchResults struct {
    Pages int
    Page int
    TLD string
    Sort string
    Query string
    Results []SearchResult
}

/*
0: Featured: None
1: Price: Low to High: price-asc-rank
2: Price: High to Low: price-desc-rank
3: Avg. Customer Review: review-rank
4: Newest Arrivals: date-desc-rank
*/

func search(tld string, searchTerm string, page int, sort string) SearchResults {
    var resultsElement SearchResults
    resultsElement.Query = searchTerm
    resultsElement.TLD = tld
    resultsElement.Page = page
    resultsElement.Sort = sort

    // build the url
    requestURL, err := url.Parse("https://amazon." + tld + "/s")
    if err != nil {
        panic(err)
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
        log.Fatalf("Error creating request %s", err)
    }

    req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 6.1; Win64; x64; rv:47.0) Gecko/20100101 Firefox/47.0")

    res, err := client.Do(req)
    if err != nil {
        log.Fatalf("Couldn't fetch search results: %s", err)
    }
    defer res.Body.Close()

    if res.StatusCode != 200 {
        log.Fatalf("Status code error: %d %s", res.StatusCode, res.Status)
    }

    // Load the HTML document
    doc, err := goquery.NewDocumentFromReader(res.Body)
    if err != nil {
        log.Fatalf("Couldn't parse HTML result %s", err)
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
        titleEl := result.Find("div.s-title-instructions-style > h2")
        title := titleEl.Text()
        link, _ := titleEl.Find("a").First().Attr("href")

        price := result.Find("span.a-price > span.a-offscreen").First().Text()
        image, _ := result.Find("img.s-image").First().Attr("src")
        type_ := result.Find("a.a-size-base.a-link-normal.s-underline-text.s-underline-link-text.s-link-style.a-text-bold").Text()

        // length of https://m.media-amazon.com : 26
        if len(image) > 26 {
            image = "/mediaproxy" + image[26:]
        }

        /*
        price := result.Find("span.a-price-whole").Text()
        priceFraction := result.Find("span.a-price-fraction").Text()
        currencySymbol := result.Find("span.a-price-symbol").Text()
        */

        if strings.Trim(title, " ") != "" {
            var res SearchResult

            /*
            fmt.Println(title)
            fmt.Println("Link:", link)
            fmt.Println("Image:", image)
            fmt.Println("Price:", price)
            */

            res.Title = title
            res.URL = link
            res.Price = price
            res.ImageURL = image
            res.Type = type_

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



    // get the result

    result := search(tld, query, page, sort)

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
    port := flag.Int("p", 8080, "Port")
    host := flag.String("h", "localhost", "Host")
    flag.Parse()

    fmt.Printf("Serving on %s:%d\n", *host, *port)
    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        if len(r.URL.Path) > 1 {
            fmt.Fprintf(w, unhandledTemplate)
            return
        }

        fmt.Fprintf(w, indexTemplate)
    })
    http.HandleFunc("/s", handleSearch)
    http.HandleFunc("/mediaproxy/", proxyMedia)

    fmt.Println(http.ListenAndServe(*host + ":" + strconv.Itoa(*port), nil))
}
