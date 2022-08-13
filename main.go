package main
import ( "fmt"
    "log"
    "strings"
    "strconv"
    "net/url"
    "net/http"
    "html/template"
    
    "github.com/PuerkitoBio/goquery"
)

type SearchResult struct {
    Title string
    URL string
    AmazonURL string
    Price string
    ImageURL string
    Type string
}

type SearchResults struct {
    Pages int
    TLD string
    Query string
    Results []SearchResult
}


func search(tld string, searchTerm string) SearchResults {
    var resultsElement SearchResults
    resultsElement.Query = searchTerm
    resultsElement.TLD = tld

    // build the url
    requestURL, err := url.Parse("https://amazon." + tld + "/s")
    if err != nil {
        panic(err)
    }

    parameters := url.Values{}
    parameters.Add("k", searchTerm)
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
    pagesStr := doc.Find("span.s-pagination-strip > span.s-pagination-item.s-pagination-disabled").Last().Text()
    pages, err := strconv.Atoi(pagesStr)
    if err != nil {
        //NOTE this also happens when there is only one page
        fmt.Println("Couldn't fetch number of pages from %s: %s", pagesStr, err)
        pages = 1
    }
    resultsElement.Pages = pages

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
            res.AmazonURL = "https://amazon." + tld + link
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

    if r.Method == "GET" {
        // get url parameters

        tld = r.URL.Query()["tld"][0]
        query = r.URL.Query()["query"][0]

    } else if r.Method == "POST" {
        //TODO redirect to GET page
        if err := r.ParseForm(); err != nil {
            fmt.Fprintf(w, "Parseform() err %s", err)
            return
        }

        // build the url
        searchURL, _ := url.Parse("/search")

        parameters := url.Values{}
        parameters.Add("tld", r.FormValue("tld"))
        parameters.Add("query", r.FormValue("query"))
        searchURL.RawQuery = parameters.Encode()

        http.Redirect(w, r, searchURL.String(), http.StatusSeeOther)
    } else {
        fmt.Fprintf(w, "Sorry, only POST or GET is supported.")
        return
    }

    result := search(tld, query)

    t, err := template.ParseFiles("templates/searchResults.html")
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
    http.HandleFunc("/search", handleSearch)

    fmt.Println(http.ListenAndServe(":8080", nil))
}
