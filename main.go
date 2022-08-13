package main
import ( "fmt"
    "log"
    "strings"
    "strconv"
    "net/url"
    "net/http"
    "html/template"
    "math"
    
    "github.com/PuerkitoBio/goquery"
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
    Query string
    Results []SearchResult

}


func search(tld string, searchTerm string, page int) SearchResults {
    var resultsElement SearchResults
    resultsElement.Query = searchTerm
    resultsElement.TLD = tld
    resultsElement.Page = page

    // build the url
    requestURL, err := url.Parse("https://amazon." + tld + "/s")
    if err != nil {
        panic(err)
    }

    parameters := url.Values{}
    parameters.Add("k", searchTerm)
    parameters.Add("page", strconv.Itoa(page))
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
        parameters.Add("page", strconv.Itoa(page))
        searchURL.RawQuery = parameters.Encode()

        http.Redirect(w, r, searchURL.String(), http.StatusSeeOther)
    } else {
        fmt.Fprintf(w, "Sorry, only POST or GET is supported.")
        return
    }



    // get the result

    result := search(tld, query, page)

    // expose simple add / subtract functions to the template
    fm := template.FuncMap{
        "substract": func(a, b int) int {
            return a - b
        },
        "add": func(a, b int) int {
            return a + b
        },
    }

    t, err := template.New("searchResults.html").Funcs(fm).ParseFiles("templates/searchResults.html")
    if err != nil {
        fmt.Println(err)
    }
    err = t.Execute(w, result)

    if err != nil {
        fmt.Println(err)
    }
}

func main() {
    fmt.Println("Serving on :8080")
    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        http.ServeFile(w, r, "templates/index.html")
    })
    http.HandleFunc("/s", handleSearch)

    fmt.Println(http.ListenAndServe(":8080", nil))
}
