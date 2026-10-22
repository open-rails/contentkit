// @vitest-environment jsdom
import '../../test/dom.js';
import {fireEvent,render,screen,waitFor} from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type {ReactNode} from 'react';
import {expect,it,vi} from 'vitest';
import {createContentKitClient} from '../../client/index.js';
import type {Content,ContentModel,ReaderMeta,ReaderPlan,ReadResult,SegmentInput,Version} from '../../client/generated/wire.js';
import {DraftHistoryProvider,useDraftHistory} from '../../react/draft-history.js';
import {ReaderManifestEditor} from '../admin/reader-manifest.js';
import {EditorSaveContext,useEditorSaveCoordinator} from './editor-save.js';
import {SegmentsEditor} from './editors.js';
import {OfferFields} from './offer-fields.js';

const hash='a'.repeat(64);
const edition:Version={id:'v1',code:'EDITION1',position:1,languages:['en'],traits:[],state:'draft',live:false,has_audio:false,entities:[],attributes:{},segments:[],decision:{allowed:true}};
const content:Content={id:'c1',code:'GALLERY1',kind:'gallery',path:'/g/GALLERY1',language:'en',title:'Gallery',state:'draft',comments:'open',live:false,access:{level:'public'},restrictions:[],attributes:{},creators:[],entities:[],versions:[edition],editor:true,decision:{allowed:true},revision:1,created_at:'2026-10-18T00:00:00Z',updated_at:'2026-10-18T00:00:00Z'};

function fixture(presentation:'reader'|'player'='reader'){
 const model:ContentModel={languages:['en'],tiers:[],signals:false,kinds:{gallery:{route:'g',presentation,versions:{languages:'one'},segments:true}},entity_kinds:{}};
 let stored:ReaderMeta={schema_version:1,anchors:[],background:[],foreground:[]};
 const writes:{path:string;body:unknown}[]=[];
 const plan=():ReaderPlan=>({content_id:'c1',version_id:'v1',publication_id:'pub1',pages:1,segments:[{start:0,end:1,released:true,locked:false,pages:[{id:hash,name:'01.png',w:570,h:806,files:{thumb:'thumb/01.webp'}}]}],previews:[],reader:stored});
 const fetch=vi.fn(async(input:RequestInfo|URL,init?:RequestInit)=>{
  const path=new URL(String(input),'https://test.invalid').pathname.replace(/^\/v1/,'');
  if((init?.method??'GET')==='GET'){
   if(path==='/kinds')return Response.json(model);
   if(path==='/contents/c1/versions/v1/reader')return Response.json(plan());
  }
  const body=JSON.parse(String(init?.body));writes.push({path,body});
  if(path==='/contents/c1/versions/v1/reader'){stored=body as ReaderMeta;return new Response(null,{status:204});}
  if(path==='/contents/c1/versions/v1/segments')return Response.json({...edition,segments:(body.segments as SegmentInput[]).map(row=>({...row,start:row.start??0,end:row.end??1,released:true}))});
  return Response.json({code:'not_found',error:'Unexpected request'},{status:404});
 });
 const client=createContentKitClient({baseUrl:'',fetch:fetch as typeof globalThis.fetch});
 const read:ReadResult={files:[{path:'pages/01.png',page_id:hash,type:'image/png',upload:true}],total:1,offset:0,limit:200,expires:0,access:'full',uploads:[{path:'pages/{name}',types:['image/png'],max_bytes:1000,page:true}]};
 vi.spyOn(client.media,'read').mockResolvedValue(read);
 return{client,writes};
}

function Coordinated({children}:{children:ReactNode}){
 return <DraftHistoryProvider><Workspace>{children}</Workspace></DraftHistoryProvider>;
}
function Workspace({children}:{children:ReactNode}){
 const coordinator=useEditorSaveCoordinator(),history=useDraftHistory()!;
 return <EditorSaveContext.Provider value={coordinator.context}>
  <button disabled={!coordinator.dirty||coordinator.pending||!!coordinator.problem()} onClick={()=>void coordinator.save(()=>true).then(problem=>{if(!problem)history.clear();})}>Save all</button>
  <button disabled={!history.canUndo} onClick={history.undo}>Undo</button>
  <button disabled={!history.canRedo} onClick={history.redo}>Redo</button>
  {children}
 </EditorSaveContext.Provider>;
}

it.each(['reader','player'] as const)('coordinated %s chapters stage, undo and save only through the parent',async presentation=>{
 const s=fixture(presentation),user=userEvent.setup();
 render(<Coordinated><SegmentsEditor content={content} version={edition} client={s.client}/></Coordinated>);
 await user.click(await screen.findByRole('button',{name:'Add chapter'}));
 const own=screen.queryByRole('button',{name:'Save chapters'});if(own)await user.click(own);
 expect(s.writes).toEqual([]);
 expect(own).toBeNull();
 await user.click(screen.getByRole('button',{name:'Undo'}));
 expect(screen.queryByLabelText('Chapter 1 title')).toBeNull();
 await user.click(screen.getByRole('button',{name:'Redo'}));
 expect(screen.getByLabelText('Chapter 1 title')).toHaveValue('');
 fireEvent.change(screen.getByLabelText('Chapter 1 title'),{target:{value:'Staged chapter'}});
 expect(s.writes).toEqual([]);
 await user.click(screen.getByRole('button',{name:'Save all'}));
 await waitFor(()=>expect(s.writes).toHaveLength(1));
 expect(s.writes[0]).toMatchObject({path:'/contents/c1/versions/v1/segments',body:{segments:[{position:1,title:'Staged chapter',...(presentation==='reader'?{start_page:hash}:{start:0,end:1})}]}});
});

it('coordinated reader metadata ignores Enter/submit and uses parent undo and Save',async()=>{
 const s=fixture(),user=userEvent.setup(),containingSubmit=vi.fn();
 render(<div onSubmit={containingSubmit}><Coordinated><ReaderManifestEditor content='c1' version='v1' client={s.client}/></Coordinated></div>);
 const input=await screen.findByLabelText('Auto-advance (seconds, optional)');
 fireEvent.change(input,{target:{value:'6'}});
 await user.click(input);await user.keyboard('{Enter}');
 fireEvent.submit(input.closest('form')!);
 expect(containingSubmit).not.toHaveBeenCalled();
 expect(s.writes).toEqual([]);
 expect(screen.queryByRole('button',{name:'Save'})).toBeNull();
 expect(screen.queryByRole('button',{name:'Revert'})).toBeNull();
 await user.click(screen.getByRole('button',{name:'Undo'}));expect(input).toHaveValue(null);
 await user.click(screen.getByRole('button',{name:'Redo'}));expect(input).toHaveValue(6);
 expect(s.writes).toEqual([]);
 await user.click(screen.getByRole('button',{name:'Save all'}));
 await waitFor(()=>expect(s.writes).toHaveLength(1));
 expect(s.writes[0]).toMatchObject({path:'/contents/c1/versions/v1/reader',body:{defaults:{auto_advance_seconds:6}}});
});

it.each([false,true])('coordinated offer edits defer selling/repricing to the parent (active=%s)',async active=>{
 const sell=vi.fn().mockResolvedValue({saved:true}),withdraw=vi.fn(),user=userEvent.setup();
 render(<Coordinated><OfferFields pending={false} offer={active?{terms:{amount:'4.99',currency:'USD'}}:undefined} onSell={sell} onWithdraw={withdraw}/></Coordinated>);
 const price=screen.getByLabelText('Price');fireEvent.change(price,{target:{value:'8.50'}});
 const own=screen.queryByRole('button',{name:active?'Change price':'Put on sale'});if(own)await user.click(own);
 expect(sell).not.toHaveBeenCalled();expect(own).toBeNull();
 await user.click(screen.getByRole('button',{name:'Undo'}));expect(price).toHaveValue(active?'4.99':'');
 await user.click(screen.getByRole('button',{name:'Redo'}));expect(price).toHaveValue('8.50');
 await user.click(screen.getByRole('button',{name:'Save all'}));
 await waitFor(()=>expect(sell).toHaveBeenCalledExactlyOnceWith({amount:'8.50',currency:'USD'}));
 expect(withdraw).not.toHaveBeenCalled();
});

it('keeps coordinated offer withdrawal as an explicit lifecycle action',async()=>{
 const sell=vi.fn(),withdraw=vi.fn(),user=userEvent.setup();
 render(<Coordinated><OfferFields pending={false} offer={{terms:{amount:'4.99',currency:'USD'}}} onSell={sell} onWithdraw={withdraw}/></Coordinated>);
 await user.click(screen.getByRole('button',{name:'Take off sale'}));
 expect(withdraw).toHaveBeenCalledOnce();expect(sell).not.toHaveBeenCalled();
});

it('keeps the standalone offer save action available',async()=>{
 const sell=vi.fn().mockResolvedValue({saved:true}),user=userEvent.setup();
 render(<OfferFields pending={false} onSell={sell} onWithdraw={vi.fn()}/>);
 fireEvent.change(screen.getByLabelText('Price'),{target:{value:'4.99'}});
 await user.click(screen.getByRole('button',{name:'Put on sale'}));
 await waitFor(()=>expect(sell).toHaveBeenCalledExactlyOnceWith({amount:'4.99',currency:'USD'}));
});
