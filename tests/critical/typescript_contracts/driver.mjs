import assert from "node:assert/strict";
import { createContentKitClient, ContentKitError } from "../../../sdk/ui/dist/client.js";

const fixture=JSON.parse(process.env.CONTENTKIT_TYPESCRIPT_FIXTURE);
const group=process.argv[2];
const owner=createContentKitClient({baseUrl:fixture.base,token:()=>fixture.owner});
const reader=createContentKitClient({baseUrl:fixture.base,token:()=>fixture.reader});
const anonymous=createContentKitClient({baseUrl:fixture.base});
const bindings=[];
function proved(...names){for(const name of names){assert(!bindings.includes(name),`duplicate semantic receipt ${name}`);bindings.push(name);}}
async function refused(action,status,code){await assert.rejects(action,error=>error instanceof ContentKitError&&error.status===status&&error.code===code);}

if(group==="catalog_publication"){
 const config=await anonymous.config();
 assert.equal(config.comment_max_length,2200);
 assert.deepEqual(config.anonymous,{comments:false,reactions:false,votes:false});
 proved("ContentKitClient.config");
 const kinds=await anonymous.kinds();
 assert.deepEqual(Object.keys(kinds.kinds).sort(),["article","gallery"]);
 assert.deepEqual(kinds.languages,["en","de"]);
 proved("ContentKitClient.kinds");
 assert.equal(owner.url("contents"),fixture.base+"/v1");
 assert.equal(owner.url("notifications"),fixture.base+"/v1/notifications");
 proved("ContentKitClient.url");
 const before=await owner.catalog.listEvents({types:["content.published"],limit:1});
 assert.equal(typeof before.next_cursor,"string");assert(before.next_cursor.length>0);
 const draft=await owner.contents.create({kind:"article",text:{en:{title:"TS Unicode Straße / ?",body:"Exact TypeScript source body"}}});
 await refused(()=>anonymous.contents.get(draft.id),404,"not_found");
 await refused(()=>reader.contents.publish(draft.id),403,"forbidden");
 assert.equal((await owner.contents.get(draft.id)).state,"draft");
 const published=await owner.contents.publish(draft.id);
 assert.equal(published.id,draft.id);assert.equal(published.live,true);
 assert.equal((await anonymous.contents.get(draft.id)).body,"Exact TypeScript source body");
 proved("ContentsClient.publish");
 const second=await owner.contents.create({kind:"article",text:{en:{title:"Second TypeScript article",body:"Ready without media"}}});
 const ready=await owner.contents.publishWhenReady(second.id,undefined,0);
 assert.equal(ready.id,second.id);assert.equal(ready.live,true);
 await refused(()=>reader.contents.publishWhenReady(second.id,undefined,0),403,"forbidden");
 assert.equal((await anonymous.contents.get(second.id)).body,"Ready without media");
 proved("ContentsClient.publishWhenReady");
 const facts=[];let cursor=before.next_cursor;
 for(let i=0;i<4;i++){
  const page=await owner.catalog.listEvents({after:cursor,types:["content.published"],limit:1});
  assert(page.events.length<=1);
  for(const fact of page.events){assert.equal(fact.type,"content.published");assert.equal(fact.scope,fixture.app);assert.match(fact.id,/^\d+$/);facts.push(fact);}
  cursor=page.next_cursor;if(!page.has_more)break;
 }
 assert.deepEqual(facts.map(x=>x.content_id),[draft.id,second.id]);
 assert.equal(new Set(facts.map(x=>x.id)).size,2);
 assert.equal((await owner.catalog.listEvents({after:cursor,types:["content.published"]})).events.length,0);
 await refused(()=>anonymous.catalog.listEvents(),401,"unauthorized");
 proved("CatalogClient.listEvents");
 const hidden=await owner.contents.unpublish(draft.id);assert.equal(hidden.id,draft.id);assert.equal(hidden.live,false);
 await refused(()=>anonymous.contents.get(draft.id),404,"not_found");
 await owner.contents.publish(draft.id);
 assert.equal((await anonymous.contents.get(draft.id)).body,"Exact TypeScript source body");
 proved("ContentsClient.unpublish");
 await refused(()=>anonymous.contents.get(fixture.legacy),404,"not_found");
 const restored=await owner.contents.restore(fixture.legacy);
 assert.equal(restored.id,fixture.legacy);assert.equal(restored.live,true);
 assert.equal((await anonymous.contents.get(fixture.legacy)).body,"Retained historical body");
 proved("ContentsClient.restore");
 const totals=await anonymous.catalog.totals({kinds:["article"],languages:["en"]});
 assert.deepEqual(Object.keys(totals.kinds),["article"]);assert.equal(totals.kinds.article.public,3);
 assert.equal(totals.kinds.article.languages.en.public,3);
 proved("CatalogClient.totals");
 await owner.contents.removeVersion(draft.id,draft.versions[0].id);
 await refused(()=>anonymous.contents.get(draft.id),404,"not_found");
 const retired=await owner.contents.manage({ids:[draft.id],state:"deleted"});
 assert.equal(retired.length,1);assert.equal(retired[0].id,draft.id);assert.equal(retired[0].state,"deleted");
 await refused(()=>owner.contents.restore(draft.id),409,"conflict");
 assert.equal((await anonymous.contents.get(second.id)).id,second.id);
 proved("ContentsClient.removeVersion");
}else throw new Error(`Unknown contract group ${group}`);
console.log(JSON.stringify({group,bindings,passed:true}));
