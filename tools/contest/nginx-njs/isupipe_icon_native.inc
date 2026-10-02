/* This content handler does not create a JavaScript request context. */
static char *
ngx_http_isupipe_icon_native(ngx_conf_t *cf, ngx_command_t *cmd, void *conf)
{
    ngx_http_js_loc_conf_t    *jlcf = conf;
    ngx_js_main_conf_t       *jmcf;
    ngx_http_core_loc_conf_t *clcf;
    ngx_str_t                *args;
    ngx_str_t                 username = ngx_string("icon_username");
    ngx_str_t                 session = ngx_string("icon_session_id");
    ngx_str_t                 cookie = ngx_string("cookie_SESSIONID");

    if (jlcf->icon_hashes != NULL || jlcf->content.len != 0) {
        return "is duplicate";
    }
    args = cf->args->elts;
    jmcf = ngx_http_conf_get_module_main_conf(cf, ngx_http_js_module);
    jlcf->icon_hashes = ngx_isupipe_icon_dict_find(jmcf->dicts, &args[1]);
    jlcf->icon_control = ngx_isupipe_icon_dict_find(jmcf->dicts, &args[2]);
    if (jlcf->icon_hashes == NULL || jlcf->icon_control == NULL
        || jlcf->icon_hashes == jlcf->icon_control)
    {
        ngx_conf_log_error(NGX_LOG_EMERG, cf, 0,
                          "isupipe_icon_native requires two existing string zones without timeout");
        return NGX_CONF_ERROR;
    }
    jlcf->icon_username_index = ngx_http_get_variable_index(cf, &username);
    jlcf->icon_session_index = ngx_http_get_variable_index(cf, &session);
    jlcf->icon_cookie_index = ngx_http_get_variable_index(cf, &cookie);
    if (jlcf->icon_username_index == NGX_ERROR
        || jlcf->icon_session_index == NGX_ERROR
        || jlcf->icon_cookie_index == NGX_ERROR)
    {
        return NGX_CONF_ERROR;
    }
    clcf = ngx_http_conf_get_module_loc_conf(cf, ngx_http_core_module);
    clcf->handler = ngx_http_isupipe_icon_handler;
    return NGX_CONF_OK;
}


static ngx_int_t
ngx_http_isupipe_icon_session(ngx_http_request_t *r,
    ngx_http_js_loc_conf_t *jlcf)
{
    ngx_http_variable_value_t *cookie, *session;
    ngx_str_t                 encoded, decoded;
    u_char                    token[65], *hex, ch;
    uint64_t                  expires;
    ngx_uint_t                i;

    cookie = ngx_http_get_indexed_variable(r, jlcf->icon_cookie_index);
    if (cookie == NULL || cookie->not_found || cookie->len != 87) {
        return NGX_OK;
    }
    for (i = 0; i < cookie->len; i++) {
        ch = cookie->data[i];
        if (!((ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')
              || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_'))
        {
            return NGX_OK;
        }
    }
    encoded.data = cookie->data;
    encoded.len = cookie->len;
    decoded.data = token;
    if (ngx_decode_base64url(&decoded, &encoded) != NGX_OK
        || decoded.len != sizeof(token) || token[0] != 1 || (token[1] & 128))
    {
        return NGX_OK;
    }
    expires = 0;
    for (i = 11; i < 17; i++) {
        expires = (expires << 8) | token[i];
    }
    if ((uint64_t) ngx_time() > expires) {
        return NGX_OK;
    }
    hex = ngx_pnalloc(r->pool, 64);
    if (hex == NULL) {
        return NGX_ERROR;
    }
    ngx_hex_dump(hex, token + 33, 32);
    /* Opaque logging identity only. Authentication remains in the Go APIs. */
    session = &r->variables[jlcf->icon_session_index];
    session->data = hex;
    session->len = 64;
    session->valid = 1;
    session->not_found = 0;
    session->no_cacheable = 0;
    return NGX_OK;
}


static ngx_int_t
ngx_http_isupipe_icon_handler(ngx_http_request_t *r)
{
    ngx_http_js_loc_conf_t    *jlcf;
    ngx_http_variable_value_t *username;
    ngx_table_elt_t           *validator, *etag;
    ngx_str_t                 name, fallback = ngx_string("@icon_reader");
    ngx_uint_t                i;
    u_char                    ch;
    ngx_int_t                 rc;

    jlcf = ngx_http_get_module_loc_conf(r, ngx_http_js_module);
    validator = r->headers_in.if_none_match;
    if (r->method != NGX_HTTP_GET || validator == NULL
        || validator->value.len != 66 || validator->value.data[0] != '"'
        || validator->value.data[65] != '"')
    {
        goto fallback;
    }
    for (i = 1; i < 65; i++) {
        ch = validator->value.data[i];
        if (!((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f'))) {
            goto fallback;
        }
    }
    username = ngx_http_get_indexed_variable(r, jlcf->icon_username_index);
    if (username == NULL || username->not_found) {
        goto fallback;
    }
    name.data = username->data;
    name.len = username->len;
    while (name.len != 0 && name.data[name.len - 1] == ' ') {
        name.len--;
    }
    if (ngx_isupipe_icon_etag_matches(jlcf->icon_hashes, jlcf->icon_control,
                                     &name, &validator->value) != NGX_OK)
    {
        goto fallback;
    }
    rc = ngx_http_discard_request_body(r);
    if (rc != NGX_OK) {
        return rc;
    }
    if (ngx_http_isupipe_icon_session(r, jlcf) != NGX_OK) {
        return NGX_HTTP_INTERNAL_SERVER_ERROR;
    }
    etag = ngx_list_push(&r->headers_out.headers);
    if (etag == NULL) {
        return NGX_HTTP_INTERNAL_SERVER_ERROR;
    }
    etag->hash = 1;
    ngx_str_set(&etag->key, "ETag");
    etag->value = validator->value;
    r->headers_out.etag = etag;
    r->headers_out.status = NGX_HTTP_NOT_MODIFIED;
    r->headers_out.content_length_n = -1;
    r->header_only = 1;
    return ngx_http_send_header(r);

fallback:
    return ngx_http_named_location(r, &fallback);
}
