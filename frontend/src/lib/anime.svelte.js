import { api, json, query } from './api.js';

export function createAnimeController({readSettings=()=>true}={}) {
  const data = $state({
    status: null, settings: null, connection: null, subscriptions: [], episodes: [], offset: 0, filter: '', subscription: 0, watch: '',
    loading: true, pending: false, error: '', message: '', task: '', taskActive: false, taskInfo: null, metadataOptions: null, historyOptions: null,
  });
  let request;
  let generation = 0;
  let disposed = false;

  async function load({ quiet = false } = {}) {
    if (disposed) return;
    const current = ++generation;
    request?.abort();
    request = new AbortController();
    const options = { signal: request.signal };
    if (!quiet) data.loading = true;
    try {
      const status = await api('/api/v1/anime/status', options);
      const [subscriptions, episodes, settings] = status.configured ? await Promise.all([
        api('/api/v1/anime/subscriptions', options),
        api(query('/api/v1/anime/episodes', { state: data.filter, limit: 50, offset: data.offset, subscription: data.subscription, watch: data.watch }), options),
        !readSettings()?Promise.resolve(null):data.settings ? Promise.resolve(data.settings) : api('/api/v1/anime/settings', options),
      ]) : [{ subscriptions: [] }, { episodes: [] }, null];
      if (disposed || current !== generation) return;
      data.status = status;
      data.settings = settings;
      data.subscriptions = subscriptions.subscriptions || [];
      data.episodes = episodes.episodes || [];
      data.error = '';
    } catch (error) {
      if (!disposed && current === generation && error.name !== 'AbortError') data.error = error.message;
    } finally {
      if (!disposed && current === generation) data.loading = false;
    }
  }

  async function mutate(path, options = { method: 'POST' }, message = '已保存。') {
    if (data.pending || disposed) return null;
    data.pending = true;
    data.error = '';
    data.message = '';
    try {
      const result = await api(path, options);
      if (disposed) return null;
      data.message = result?.job_id ? message + '可在任务中心查看结果。' : message;
      if (result?.job_id) {data.task = result.job_id; data.taskActive = true; data.taskInfo = null;}
      await load({ quiet: true });
      return result;
    } catch (error) {
      if (!disposed) data.error = error.message;
      return null;
    } finally {
      if (!disposed) data.pending = false;
    }
  }

  return {
    data,
    load,
    updateTask(result) {
      if (result.job?.id !== data.task) return;
      data.taskActive = ['queued','running','waiting','paused'].includes(result.job.state);
      for (const event of result.events || []) {if (event.to !== 'running' || !event.note?.startsWith('{')) continue; try {const info = JSON.parse(event.note); if (['anime.metadata','anime.history.cleanup'].includes(info.kind)) data.taskInfo = info;} catch { /* Plain task notes are also valid. */ }}
    },
    async refreshMetadata(subscription,options) {
      const result = await mutate(`/api/v1/anime/subscriptions/${subscription}/metadata`,json('POST',options,{headers:{'X-Nasctl-Confirm':'1'}}),'元数据更新任务已创建。');
      if (result?.job_id) data.metadataOptions = {subscription,...options};
      return result;
    },
    async cleanupHistory(subscription,options) {
      const result = await mutate(`/api/v1/anime/subscriptions/${subscription}/history/cleanup`,json('POST',options,{headers:{'X-Nasctl-Confirm':'1'}}),'历史清理任务已创建。');
      if (result?.job_id) data.historyOptions = {subscription,...options};
      return result;
    },
    refresh: (id) => mutate(query('/api/v1/anime/refresh', { subscription: id }), { method: 'POST' }, '订阅刷新任务已创建。'),
    sync: (id) => mutate(query('/api/v1/anime/sync', { subscription: id }), { method: 'POST' }, '下载同步任务已创建。'),
    ignore: (episode) => mutate(`/api/v1/anime/episodes/${episode.id}/ignore`, { method: 'POST' }, '已忽略本集，可重新选择下载。'),
    controlDownload: (episode,action) => mutate(`/api/v1/anime/episodes/${episode.id}/download`,json('POST',{action},{headers:{'X-Nasctl-Confirm':'1'}}),'下载器操作请求已发送。'),
    async checkConnection() {
      if (data.pending || disposed) return;
      data.pending = true; data.error = '';
      try { const result = await api('/api/v1/anime/connection', { method: 'POST' }); if (!disposed) data.connection = result; }
      catch (error) { if (!disposed) data.error = error.message; }
      finally { if (!disposed) data.pending = false; }
    },
    save: (subscription) => mutate('/api/v1/anime/subscriptions' + (subscription.id ? '/' + subscription.id : ''), json(subscription.id ? 'PUT' : 'POST', subscription)),
    setEnabled: (subscription) => mutate(`/api/v1/anime/subscriptions/${subscription.id}/enabled`,json('POST',{enabled:!subscription.enabled}),subscription.enabled ? '已暂停订阅，下载器中已提交的任务仍由下载器管理。' : '已启用订阅，将按原有自动下载设置处理。'),
    archive: (subscription) => mutate(`/api/v1/anime/subscriptions/${subscription.id}/archive`,json('POST',{archived:!subscription.archived_at}),subscription.archived_at ? '订阅已恢复并保持暂停，请核对自动下载设置后启用。' : '订阅已归档，剧集与观看记录保留。'),
    markWatched: (episode) => mutate(`/api/v1/anime/episodes/${episode.id}/playback`,json('PUT',episode.watched ? {watched:false,position:0,duration:0} : {watched:true}),episode.watched ? '已标记为未看，播放位置已重置。' : '已标记为看完。'),
    async queue(episode) {
      const result = await mutate(`/api/v1/anime/episodes/${episode.id}/queue`);
      if (result) return this.sync(episode.subscription_id);
      return null;
    },
    filter(value) { data.filter = value; data.watch = ''; data.offset = 0; return load(); },
    watch(value) { data.watch = value; if (value) data.filter = 'archived'; data.offset = 0; return load(); },
    selectSubscription(id) { data.subscription = Number(id) || 0; data.offset = 0; return load(); },
    page(offset) { data.offset = offset; return load(); },
    dispose() { disposed = true; ++generation; request?.abort(); },
  };
}
