package api

import (
	"net/http"

	"github.com/julienschmidt/httprouter"
	"telemetry.gosuda.org/telemetry/internal/types"
)

var _gopackage_html = []byte(
	`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta http-equiv="X-UA-Compatible" content="IE=edge">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>telemetry.gosuda.org/telemetry</title>
    <meta name="og:title" content="telemetry.gosuda.org/telemetry">
    <meta name="og:description" content="telemetry.gosuda.org/telemetry">
    <meta name="description" content="telemetry.gosuda.org/telemetry">
    <meta name="go-import" content="telemetry.gosuda.org/telemetry git https://github.com/gosuda/telemetry.gosuda.org.git">
</head>
<body>
	<style>
		.box {
			margin: 0 auto;
			width: 80%;
			text-align: center;
		}
	</style>
    <div class="box">
        <h1>telemetry.gosuda.org/telemetry</h1>
        <br/>
        <p>Documentation for this module is available at <a href="https://pkg.go.dev/telemetry.gosuda.org/telemetry">telemetry.gosuda.org/telemetry</a></p>
    </div>
</body>
</html>
`)

func GoPackageHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write(_gopackage_html)
	}
}
