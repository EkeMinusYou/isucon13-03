/* Native readers of the dictionaries published by icon.js. */
ngx_js_dict_t *
ngx_isupipe_icon_dict_find(ngx_js_dict_t *dicts, ngx_str_t *name)
{
    ngx_js_dict_t *dict;

    for (dict = dicts; dict != NULL; dict = dict->next) {
        if (dict->shm_zone->shm.name.len == name->len
            && ngx_memcmp(dict->shm_zone->shm.name.data, name->data, name->len) == 0
            && dict->type == NGX_JS_DICT_TYPE_STRING && dict->timeout == 0)
        {
            return dict;
        }
    }
    return NULL;
}


ngx_int_t
ngx_isupipe_icon_etag_matches(ngx_js_dict_t *hashes, ngx_js_dict_t *control,
    ngx_str_t *name, ngx_str_t *etag)
{
    njs_str_t            active_key = njs_str("active"), user_key;
    ngx_js_dict_node_t  *node;
    u_char               before[64];
    size_t               length;
    ngx_int_t            matched;
    off_t                expires;
    struct timeval       tv;

    ngx_rwlock_rlock(&control->sh->rwlock);
    node = ngx_js_dict_lookup(control, &active_key);
    if (node == NULL || node->u.value.len <= 37
        || node->u.value.len > sizeof(before) || node->u.value.data[36] != ':')
    {
        ngx_rwlock_unlock(&control->sh->rwlock);
        return NGX_DECLINED;
    }
    length = node->u.value.len;
    ngx_memcpy(before, node->u.value.data, length);
    ngx_rwlock_unlock(&control->sh->rwlock);

    expires = ngx_atoof(before + 37, length - 37);
    if (expires == NGX_ERROR) {
        return NGX_DECLINED;
    }

    user_key.start = name->data;
    user_key.length = name->len;
    ngx_rwlock_rlock(&hashes->sh->rwlock);
    node = ngx_js_dict_lookup(hashes, &user_key);
    matched = node != NULL && node->u.value.len == 64
              && ngx_memcmp(node->u.value.data, etag->data + 1, 64) == 0;
    ngx_rwlock_unlock(&hashes->sh->rwlock);
    if (!matched) {
        return NGX_DECLINED;
    }

    /* No pointer into shared memory survives a read lock. The publication
     * identity prevents an entry read during a concurrent replacement. */
    ngx_rwlock_rlock(&control->sh->rwlock);
    node = ngx_js_dict_lookup(control, &active_key);
    matched = node != NULL && node->u.value.len == length
              && ngx_memcmp(node->u.value.data, before, length) == 0;
    ngx_rwlock_unlock(&control->sh->rwlock);
    ngx_gettimeofday(&tv);
    return matched && (uint64_t) expires > (uint64_t) tv.tv_sec * 1000
                                             + tv.tv_usec / 1000
           ? NGX_OK : NGX_DECLINED;
}
