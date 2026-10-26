// Exercise the built package, so stale exports and bundle drift are observable.
import React from "../../../sdk/ui/node_modules/react/index.js";
import { createRoot } from "../../../sdk/ui/node_modules/react-dom/client.js";
import { createContentKitClient } from "../../../sdk/ui/dist/client.js";
import { ContentKitProvider } from "../../../sdk/ui/dist/react/provider.js";
import { useContentDetail } from "../../../sdk/ui/dist/react/detail.js";
import { useFavorite } from "../../../sdk/ui/dist/react/engagement.js";
import { ChannelFeed, RepostButton, ContentEditor } from "../../../sdk/ui/dist/index.js";

let fixture, bearer, currentViewer, client, root;
const target = () => ({ type: "contents", id: fixture.item });
function FavoriteView() {
  const state = useFavorite(target());
  window.favoriteSamples ??= [];
  window.favoriteSamples.push({viewer:currentViewer,loading:state.loading,favorited:state.favorited,error:state.error?.code});
  return React.createElement("output", { id: "favorite", "data-viewer": currentViewer, "data-loading": String(state.loading) }, state.error?.code ?? String(state.favorited));
}
function VersionReader({ version }) {
  const detail = useContentDetail(fixture.item, { version, language: "en" });
  return React.createElement("output", { id: "version-reader", "data-loading": String(detail.loading), "data-version": detail.version?.id }, detail.content?.body ?? "");
}
function render() {
  root ??= createRoot(document.querySelector("#root"));
  root.render(React.createElement(ContentKitProvider, { client, viewer: currentViewer }, React.createElement(FavoriteView)));
}
window.critical = {
  initialize(input) { fixture=input; bearer=input.aliceToken; currentViewer=input.alice; client=createContentKitClient({baseUrl:"/content",token:()=>bearer}); window.client=client; },
  mount() { render(); },
  mountVersionEditor() {
    root ??= createRoot(document.querySelector("#root"));
    root.render(React.createElement(ContentKitProvider, { client, viewer: currentViewer, language: "en" },
      React.createElement(ContentEditor, { id: fixture.item, kind: "article", languages: [{value:"en",label:"English"},{value:"zh",label:"Chinese"}] })));
  },
  mountVersionReader(version) {
    root ??= createRoot(document.querySelector("#root"));
    root.render(React.createElement(ContentKitProvider, { client, viewer: currentViewer, language: "en" }, React.createElement(VersionReader, {version})));
  },
  mountChannel() {
    root ??= createRoot(document.querySelector("#root"));
    root.render(React.createElement(ContentKitProvider, { client, viewer: currentViewer, language: "en" },
      React.createElement(React.Fragment, null,
        React.createElement(RepostButton, { contentId: fixture.item, originalChannelId: fixture.originalChannel }),
        React.createElement(ChannelFeed, { channelId: fixture.destination, query: { language: "en" }, empty: React.createElement("p", { id: "empty-channel" }, "Empty channel") }))));
  },
  async uploadChannelImage() {
    const ref=client.channels.folder(fixture.destination);
    if(ref.kind!=="channel"||ref.id!==fixture.destination)throw Error("channel media reference changed identity");
    const blob=await(await fetch("/fixture.png")).blob();
    const updated=await client.channels.uploadImage(fixture.destination,"avatar",new File([blob],"avatar.png",{type:"image/png"}),{timeout:20000});
    if(!updated.avatar?.renditions?.length)throw Error("channel avatar did not complete processing");
    return updated.id;
  },
  switchToBob() {
    window.leaks=[];
    window.observer=new MutationObserver(()=>{const view=document.querySelector("#favorite");if(view?.dataset.viewer===fixture.bob&&view.textContent==="true")window.leaks.push(view.textContent);});
    window.observer.observe(document.querySelector("#root"),{subtree:true,childList:true,characterData:true,attributes:true});
    bearer=fixture.bobToken; currentViewer=fixture.bob; render();
  },
  switchScope() { bearer=fixture.bobToken;client.setReadScope("bob"); },
  async binding() {
    const created=await client.contents.create({kind:"article",versions:[{languages:["en"],text:{en:{title:"Browser Straße & slash / query?",body:"Browser body"}}}]});
    const byVersion=await client.contents.getByVersion(created.versions[0].id);
    if(byVersion.id!==created.id || byVersion.body!=="Browser body")throw Error("TS exact version/body binding lost");
    const events=[];const stop=client.subscribe(event=>events.push(event));
    try { await client.contents.create({kind:"unknown-kind",versions:[{languages:["en"],text:{en:{title:"must refuse"}}}]});throw Error("invalid kind accepted"); }
    catch(error) { if(error.status!==400 || error.code!=="invalid_request")throw error; }
    stop();if(events.length)throw Error("failed mutation emitted success");
    return created.id;
  },
  beginMutation() { client.setReadScope("alice");window.changes=[];client.subscribe((change,scope)=>window.changes.push({change,scope}));window.mutation=client.favorites.set(target(),false); },
  async finishMutation() { await window.mutation;return window.changes; },
  beginCancellation() { window.abort=new AbortController();window.cancelled=client.contents.get(fixture.item,undefined,window.abort.signal).then(()=>({code:"unexpected_success"}),error=>({code:error.code,status:error.status})); },
  cancel() {window.abort.abort();},
  async cancellation() {return window.cancelled;},
  async imageUpload() {
    const created=await client.contents.create({kind:"gallery",versions:[{languages:["en"],text:{en:{title:"Browser accepted draft"}}}]});
    const ref={kind:"gallery",id:created.id,version:created.versions[0].id};
    const blob=await (await fetch("/fixture.png")).blob();
    await client.media.put(new File([blob],"one.png",{type:"image/png"}),{ref,path:"pages/one.png",timeout:20000});
    const read=await client.media.read(ref,{editor:true});
    const output=read.files.find(file=>file.path==="reader/one.webp");
    if(!output?.url || output.w!==48 || output.h!==32)throw Error("TS ingest missing actual image output");
    return created.id;
  },
  async stableErrors() {
    const anonymous=createContentKitClient({baseUrl:"/content"});
    try {await anonymous.favorites.mine();throw Error("anonymous private listing accepted");}
    catch(error) {if(error.status!==401||error.code!=="unauthorized"||!error.refusal)throw error;}
    const created=await client.contents.create({kind:"gallery",versions:[{languages:["en"],text:{en:{title:"Typed upload refusal"}}}]});
    try {await client.media.api.presign({ref:{kind:"gallery",id:created.id,version:created.versions[0].id},path:"pages/large.png",type:"image/png",size:2097152,sha256:"ab".repeat(32)});throw Error("oversize upload accepted");}
    catch(error) {if(error.status!==413||error.code!=="too_large"||error.transient||!error.refusal)throw error;return {status:error.status,code:error.code,details:error.details};}
  },
};
