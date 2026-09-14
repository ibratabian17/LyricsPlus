export const corsHeaders = {
    "Access-Control-Allow-Origin": "*",
    "Access-Control-Allow-Methods": "GET,HEAD,POST,PUT,DELETE,OPTIONS",
    "Access-Control-Allow-Headers": "Content-Type, Authorization",
    "Access-Control-Max-Age": "86400",
};

export function handleOptions(request) {
    let headers = request.headers;
    if (
        headers.get("Origin") !== null &&
        headers.get("Access-Control-Request-Method") !== null &&
        headers.get("Access-Control-Request-Headers") !== null
    ) {
        return new Response(null, {
            headers: {
                ...corsHeaders,
                "Access-Control-Allow-Headers": headers.get("Access-Control-Request-Headers"),
            },
        });
    }
    return new Response(null, {
        headers: {
            Allow: "GET, HEAD, POST, PUT, DELETE, OPTIONS",
        },
    });
}